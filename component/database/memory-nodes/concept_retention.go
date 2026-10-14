package memoryNodes

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/language/ast"
)

// RetentionPolicy is declared on a concept. A nil policy changes nothing.
// TTL measures stored version time, not a user-controlled payload field.
type RetentionPolicy struct {
	TTL        time.Duration `json:"ttl"`
	KeepLatest bool          `json:"keepLatest"`
	Archive    string        `json:"archive"`
}

func parseRetentionAttr(attr *ast.Attribute) (*RetentionPolicy, error) {
	p := &RetentionPolicy{KeepLatest: true, Archive: "required"}
	ttl, ok := attr.Args["ttl"].(string)
	if !ok || strings.TrimSpace(ttl) == "" {
		return nil, fmt.Errorf("@retention requires ttl=\"30d\" or a positive duration")
	}
	var err error
	if strings.HasSuffix(ttl, "d") {
		var days int64
		days, err = strconv.ParseInt(strings.TrimSuffix(ttl, "d"), 10, 64)
		if err == nil && days > 0 && days <= 36500 {
			p.TTL = time.Duration(days) * 24 * time.Hour
		} else {
			err = fmt.Errorf("days must be between 1 and 36500")
		}
	} else {
		p.TTL, err = time.ParseDuration(ttl)
	}
	if err != nil || p.TTL < time.Second || p.TTL > 36500*24*time.Hour {
		return nil, fmt.Errorf("@retention ttl must be between 1s and 36500d")
	}
	if v, written := attr.Args["keepLatest"]; written {
		var valid bool
		p.KeepLatest, valid = v.(bool)
		if !valid {
			return nil, fmt.Errorf("@retention keepLatest must be true or false")
		}
	}
	if v, written := attr.Args["archive"]; written {
		p.Archive, ok = v.(string)
		if !ok || (p.Archive != "required" && p.Archive != "none") {
			return nil, fmt.Errorf("@retention archive must be \"required\" or \"none\"")
		}
	}
	return p, nil
}

func validateRetentionStore(concept string, p *RetentionPolicy) error {
	if p == nil {
		return nil
	}
	if concept == "v1:observability:logLine" && (p.TTL%(24*time.Hour) != 0 || p.KeepLatest || p.Archive != "required") {
		return fmt.Errorf("@retention on logLine requires whole days, keepLatest=false and archive=\"required\"")
	}
	return nil
}
