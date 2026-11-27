package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/livekit"
	"github.com/znasllc-io/memql/component/calls"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const mediaSignalGuardHeader = "X-MemQL-Media-Signal"
const mediaSignalLimit = 1 << 20

// MediaSignalPaths is a fixed LiveKit signalling boundary, not a destination proxy.
// These four GET routes authenticate the native media pass themselves. RTP never
// crosses the engine. The media edge translates the provider's exact route names.
func MediaSignalPaths() []string {
	var out []string
	for _, path := range []string{"/media/rtc", "/media/rtc/validate", "/media/rtc/v1", "/media/rtc/v1/validate"} {
		out = append(out, pathsWithBase(path)...)
	}
	return out
}

type mediaSignalHandler struct{ service calls.MediaSignalService }

func NewMediaSignalHandler(service calls.MediaSignalService) http.Handler {
	return &mediaSignalHandler{service: service}
}

func (h *mediaSignalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	path := ""
	for _, providerPath := range []string{"/rtc", "/rtc/validate", "/rtc/v1", "/rtc/v1/validate"} {
		for _, p := range pathsWithBase("/media" + providerPath) {
			if r.URL.Path == p {
				path = providerPath
			}
		}
	}
	// Route mounts are exact; reconstruct the provider suffix without accepting
	// caller-supplied destinations, encoded separators or body/form credentials.
	if r.Method != http.MethodGet || path == "" || r.URL.RawPath != "" || len(r.URL.RawQuery) > mediaSignalQueryLimit || r.ContentLength != 0 || h.service == nil {
		http.NotFound(w, r)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	token := q.Get("access_token")
	if bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); bearer != r.Header.Get("Authorization") {
		if token != "" && token != bearer {
			http.NotFound(w, r)
			return
		}
		token = bearer
	}
	if token == "" || len(token) > 16384 || len(q["access_token"]) > 1 {
		http.NotFound(w, r)
		return
	}
	budget := newMediaSignalBudget(time.Now())
	if !budget.join(time.Now(), q) {
		http.NotFound(w, r)
		return
	}
	validate := strings.HasSuffix(path, "/validate")
	if !validate && !websocket.IsWebSocketUpgrade(r) {
		http.NotFound(w, r)
		return
	}
	if validate {
		// SDK validation is an authority check, not a join. In particular, it
		// must never replace or drain an existing native signalling lease.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if h.service.CheckMediaSignal(ctx, token) != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "success")
		return
	}
	admission, err := h.service.BeginMediaSignal(r.Context(), token)
	if err != nil || admission == nil || admission.Forward == nil || admission.Confirm == nil || admission.Close == nil {
		http.NotFound(w, r)
		return
	}
	definitive := true // remains true until the first upstream request may be sent
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = admission.Close(ctx, definitive)
	}()
	u, err := url.Parse(admission.UpstreamURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") || admission.UpstreamGuard == "" {
		http.NotFound(w, r)
		return
	}
	u.Path = path
	q.Del("access_token")
	// Provider v0 publish can rewrite the participant identity. Native admission
	// names exactly one identity; never permit this alternate identity spelling.
	q.Del("publish")
	u.RawQuery = q.Encode()
	headers := http.Header{"Authorization": {"Bearer " + admission.UpstreamToken}, mediaSignalGuardHeader: {admission.UpstreamGuard}}
	deadline := time.Now().Add(20 * time.Second)
	if admission.ExpiresAt.Before(deadline) {
		deadline = admission.ExpiresAt
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	if u.Scheme == "http" {
		u.Scheme = "ws"
	} else {
		u.Scheme = "wss"
	}
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second, Proxy: nil}
	var upstream *websocket.Conn
	err = admission.Forward(ctx, func(forward context.Context) error {
		definitive = false
		var response *http.Response
		var dialErr error
		upstream, response, dialErr = dialer.DialContext(forward, u.String(), headers)
		if dialErr != nil && response != nil {
			definitive = true
			response.Body.Close()
		}
		return dialErr
	})
	if upstream != nil {
		defer upstream.Close()
	}
	if err != nil || upstream == nil {
		http.NotFound(w, r)
		return
	}
	// Gorilla's defaults write pong/close control frames from its reader. Keep
	// those implicit writes behind the same authority gate as data frames.
	upstream.SetCloseHandler(func(int, string) error { return nil })
	upstream.SetPingHandler(func(payload string) error {
		bounded, stop := context.WithTimeout(r.Context(), 5*time.Second)
		defer stop()
		return admission.Forward(bounded, func(forward context.Context) error {
			deadline, _ := forward.Deadline()
			return upstream.WriteControl(websocket.PongMessage, []byte(payload), deadline)
		})
	})
	upstream.SetReadLimit(mediaSignalLimit)
	_ = upstream.SetReadDeadline(deadline)
	kind, first, err := upstream.ReadMessage()
	if err != nil || !mediaSignalInitial(kind, first, admission.Room, admission.Identity) {
		// A provider Leave is a definitive refusal. A broken or unexpected
		// initial frame remains uncertain and must retain its pending fence.
		var refusal livekit.SignalResponse
		if err == nil && mediaSignalDecode(kind, first, &refusal) == nil && refusal.GetLeave() != nil {
			definitive = true
		}
		http.NotFound(w, r)
		return
	}
	definitive = true
	var initial livekit.SignalResponse
	if mediaSignalDecode(kind, first, &initial) != nil || !budget.provider(&initial, admission.Identity) {
		http.NotFound(w, r)
		return
	}
	if err = admission.Confirm(ctx); err != nil {
		http.NotFound(w, r)
		return
	}
	// The pinned provider sends its own authoritative participant inventory
	// immediately after ReconnectResponse. Read that bounded inventory before
	// releasing the socket, so a resume cannot reset the negotiation count for
	// already published tracks. Unknown pending publications remain a separate
	// provider/native durability constraint; client SyncState is never authority.
	var buffered []mediaSignalFrame
	if initial.GetReconnect() != nil {
		buffered, err = mediaSignalResumeInventory(upstream, budget, admission.Identity)
		if err != nil || admission.Forward(ctx, func(context.Context) error { return nil }) != nil {
			http.NotFound(w, r)
			return
		}
	}
	_ = upstream.SetReadDeadline(time.Time{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	downstream, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer downstream.Close()
	downstream.SetReadLimit(mediaSignalLimit)
	_ = downstream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if downstream.WriteMessage(kind, first) != nil {
		return
	}
	for _, frame := range buffered {
		if downstream.WriteMessage(frame.kind, frame.data) != nil {
			return
		}
	}
	mediaSignalRelay(r.Context(), admission, downstream, upstream, budget)
}

func mediaSignalInitial(kind int, data []byte, room, identity string) bool {
	var response livekit.SignalResponse
	if mediaSignalDecode(kind, data, &response) != nil {
		return false
	}
	if joined := response.GetJoin(); joined != nil {
		return joined.GetRoom().GetName() == room && joined.GetParticipant().GetIdentity() == identity
	}
	return response.GetReconnect() != nil
}

func mediaSignalDecode(kind int, data []byte, message proto.Message) error {
	switch kind {
	case websocket.BinaryMessage:
		return proto.Unmarshal(data, message)
	case websocket.TextMessage:
		return protojson.Unmarshal(data, message)
	default:
		return errors.New("unsupported signal frame")
	}
}

func mediaSignalRelay(ctx context.Context, admission *calls.MediaSignalAdmission, downstream, upstream *websocket.Conn, budget *mediaSignalBudget) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	refuse := func() {
		_ = downstream.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "signalling budget exceeded"), time.Now().Add(time.Second))
	}
	downstream.SetPingHandler(func(payload string) error {
		if !budget.control(time.Now(), len(payload)) {
			refuse()
			return errors.New("signal control budget exceeded")
		}
		return downstream.WriteControl(websocket.PongMessage, []byte(payload), time.Now().Add(time.Second))
	})
	downstream.SetPongHandler(func(payload string) error {
		if !budget.control(time.Now(), len(payload)) {
			refuse()
			return errors.New("signal control budget exceeded")
		}
		return nil
	})
	var writes sync.Mutex
	forward := func(operation func(context.Context) error) error {
		writes.Lock()
		defer writes.Unlock()
		bounded, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		return admission.Forward(bounded, operation)
	}
	done := make(chan struct{}, 3)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			kind, data, err := downstream.ReadMessage()
			if err != nil {
				return
			}
			if !budget.frame(time.Now(), len(data)) {
				refuse()
				return
			}
			var request livekit.SignalRequest
			if mediaSignalDecode(kind, data, &request) != nil || request.Message == nil || !budget.request(time.Now(), &request) {
				refuse()
				return
			}
			if forward(func(ctx context.Context) error {
				deadline, _ := ctx.Deadline()
				_ = upstream.SetWriteDeadline(deadline)
				return upstream.WriteMessage(kind, data)
			}) != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			kind, data, err := upstream.ReadMessage()
			if err != nil {
				return
			}
			var response livekit.SignalResponse
			if mediaSignalDecode(kind, data, &response) != nil || !budget.provider(&response, admission.Identity) {
				refuse()
				return
			}
			_ = downstream.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if downstream.WriteMessage(kind, data) != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if forward(func(context.Context) error { return nil }) != nil {
					return
				}
			}
		}
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
	cancel()
	downstream.Close()
	upstream.Close()
}

type mediaSignalFrame struct {
	kind int
	data []byte
}

func mediaSignalResumeInventory(upstream *websocket.Conn, budget *mediaSignalBudget, identity string) ([]mediaSignalFrame, error) {
	var frames []mediaSignalFrame
	size := 0
	for len(frames) < 16 {
		kind, data, err := upstream.ReadMessage()
		if err != nil {
			return nil, err
		}
		size += len(data)
		if size > mediaSignalLimit {
			break
		}
		var response livekit.SignalResponse
		if mediaSignalDecode(kind, data, &response) != nil || !budget.provider(&response, identity) {
			break
		}
		frames = append(frames, mediaSignalFrame{kind, data})
		if update := response.GetUpdate(); update != nil {
			for _, participant := range update.Participants {
				if participant.Identity == identity {
					budget.mu.Lock()
					accepted := budget.seed(participant)
					budget.mu.Unlock()
					if accepted {
						return frames, nil
					}
					return nil, errors.New("resume publication inventory exceeds envelope")
				}
			}
		}
	}
	return nil, errors.New("resume publication inventory unavailable")
}
