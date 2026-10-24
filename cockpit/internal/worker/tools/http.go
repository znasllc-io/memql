package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// runHTTPFetch implements workerHost.http_fetch with SSRF hardening
// (private network blocking via the policy), redirect cap, and
// body cap.
func runHTTPFetch(ctx context.Context, args map[string]any, policy *Policy) (*memqlv1.Success, *memqlv1.Failure) {
	urlStr := strings.TrimSpace(argString(args, "url"))
	if urlStr == "" {
		return nil, failure("bad_request", "http_fetch: url required")
	}
	if err := policy.CheckURL(urlStr); err != nil {
		return nil, failure("http_blocked", err.Error())
	}
	method := strings.ToUpper(strings.TrimSpace(argString(args, "method")))
	if method == "" {
		method = http.MethodGet
	}

	body := argString(args, "body")
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	timeoutSec := argInt(args, "timeoutSec", 30)
	if timeoutSec <= 0 {
		timeoutSec = 30
	}

	client := &http.Client{
		Timeout: time.Duration(timeoutSec) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= policy.MaxRedirects() {
				return errors.New("max redirects reached")
			}
			if err := policy.CheckURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		return nil, failure("bad_request", err.Error())
	}
	if hdrs := argMap(args, "headers"); hdrs != nil {
		for k, v := range hdrs {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, failure("http_failed", err.Error())
	}
	defer resp.Body.Close()

	limit := int64(policy.MaxBodyBytes())
	limited := io.LimitReader(resp.Body, limit+1)
	respBody, err := io.ReadAll(limited)
	if err != nil {
		return nil, failure("http_failed", err.Error())
	}
	if int64(len(respBody)) > limit {
		respBody = respBody[:limit]
		return successJSON(map[string]any{
			"status":    resp.StatusCode,
			"truncated": true,
			"body":      string(respBody),
			"headers":   flattenHeaders(resp.Header),
			"sizeLimit": limit,
		}, 0, 0, len(respBody), truncate(string(respBody), 1024)), nil
	}
	return successJSON(map[string]any{
		"status":  resp.StatusCode,
		"body":    string(respBody),
		"headers": flattenHeaders(resp.Header),
	}, 0, 0, len(respBody), truncate(string(respBody), 1024)), nil
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if len(vals) == 0 {
			continue
		}
		out[k] = fmt.Sprintf("%v", vals)
	}
	return out
}
