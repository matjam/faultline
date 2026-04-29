package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// fakeKobold returns an httptest.Server that mimics the subset of KoboldCpp
// endpoints we use, plus counters for verifying call patterns.
type fakeKobold struct {
	srv *httptest.Server

	versionCalls    atomic.Int32
	tokencountCalls atomic.Int32
	abortCalls      atomic.Int32
	perfCalls       atomic.Int32

	// Configurable behaviors.
	versionStatus   int    // override 200
	versionPayload  string // override default {"version":"2025.06.03"}
	tokencountValue int    // value returned by /api/extra/tokencount
	tokencountDelay time.Duration
	failTokencount  bool
}

func newFakeKobold() *fakeKobold {
	f := &fakeKobold{
		versionStatus:   200,
		tokencountValue: 42,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/extra/version", func(w http.ResponseWriter, r *http.Request) {
		f.versionCalls.Add(1)
		if f.versionStatus != 200 {
			w.WriteHeader(f.versionStatus)
			return
		}
		body := f.versionPayload
		if body == "" {
			body = `{"result":"KoboldCpp","version":"2025.06.03"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/api/extra/tokencount", func(w http.ResponseWriter, r *http.Request) {
		f.tokencountCalls.Add(1)
		if f.tokencountDelay > 0 {
			time.Sleep(f.tokencountDelay)
		}
		if f.failTokencount {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Echo a fixed value (or a length-derived one if 0 was passed).
		val := f.tokencountValue
		if val == 0 {
			var req struct {
				Prompt string `json:"prompt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			val = len(req.Prompt)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"value":`+itoa(val)+`}`)
	})
	mux.HandleFunc("/api/extra/abort", func(w http.ResponseWriter, r *http.Request) {
		f.abortCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true}`)
	})
	mux.HandleFunc("/api/extra/perf", func(w http.ResponseWriter, r *http.Request) {
		f.perfCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"last_process_time": 0.5,
			"last_eval_time":    1.5,
			"last_token_count":  150,
			"last_input_count":  3000,
			"last_process_speed": 6000.0,
			"last_eval_speed":   100.0,
			"total_gens":        17,
			"queue":             0,
			"idle":              1,
			"stop_reason":       1,
			"uptime":            7200
		}`)
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeKobold) Close() { f.srv.Close() }

// itoa avoids importing strconv just for this single use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// quietLogger returns a logger that drops everything; useful when we don't
// want test output cluttered by our debug prints.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestKoboldExtras_Detect(t *testing.T) {
	f := newFakeKobold()
	defer f.Close()

	// Note: we pass the fake server's URL with a /v1 suffix to confirm the
	// constructor strips it correctly when computing the kcpp root.
	k := NewKoboldExtras(f.srv.URL+"/v1", quietLogger())
	if err := k.Detect(context.Background()); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !k.Detected() {
		t.Error("Detected() should be true after successful Detect")
	}
	if k.Version() != "2025.06.03" {
		t.Errorf("Version = %q, want 2025.06.03", k.Version())
	}
	if got := f.versionCalls.Load(); got != 1 {
		t.Errorf("expected 1 version call, got %d", got)
	}
}

func TestKoboldExtras_Detect_NotKobold(t *testing.T) {
	// A server that returns 404 for /api/extra/version simulates a non-kcpp
	// OpenAI-compat backend.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	k := NewKoboldExtras(srv.URL, quietLogger())
	if err := k.Detect(context.Background()); err == nil {
		t.Error("expected detection to fail for non-kcpp endpoint")
	}
	if k.Detected() {
		t.Error("Detected() should be false after failed Detect")
	}
}

func TestKoboldExtras_Detect_EmptyVersion(t *testing.T) {
	f := newFakeKobold()
	defer f.Close()
	f.versionPayload = `{"result":"KoboldCpp","version":""}`

	k := NewKoboldExtras(f.srv.URL, quietLogger())
	if err := k.Detect(context.Background()); err == nil {
		t.Error("expected detection to fail when version field is empty")
	}
}

func TestKoboldExtras_CountString_NotDetected(t *testing.T) {
	// Without Detect() the client must fall back to the heuristic.
	k := NewKoboldExtras("http://nonexistent.invalid", quietLogger())
	if got := k.CountString(context.Background(), "abcd"); got != 1 {
		t.Errorf("undetected client should fall back to EstimateTokens; got %d, want 1", got)
	}
}

func TestKoboldExtras_CountString(t *testing.T) {
	f := newFakeKobold()
	defer f.Close()
	f.tokencountValue = 7

	k := NewKoboldExtras(f.srv.URL, quietLogger())
	if err := k.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := k.CountString(context.Background(), "hello world"); got != 7 {
		t.Errorf("CountString = %d, want 7", got)
	}
	if got := f.tokencountCalls.Load(); got != 1 {
		t.Errorf("expected 1 tokencount call, got %d", got)
	}
}

func TestKoboldExtras_CountString_EmptyShortCircuits(t *testing.T) {
	f := newFakeKobold()
	defer f.Close()

	k := NewKoboldExtras(f.srv.URL, quietLogger())
	if err := k.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := k.CountString(context.Background(), ""); got != 0 {
		t.Errorf("empty string should be 0, got %d", got)
	}
	if got := f.tokencountCalls.Load(); got != 0 {
		t.Error("empty string must not hit the network")
	}
}

func TestKoboldExtras_CountString_FallbackOnError(t *testing.T) {
	f := newFakeKobold()
	defer f.Close()
	f.failTokencount = true

	k := NewKoboldExtras(f.srv.URL, quietLogger())
	if err := k.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// "abcdefgh" is 8 chars / 4 = 2 tokens via heuristic.
	if got := k.CountString(context.Background(), "abcdefgh"); got != 2 {
		t.Errorf("expected heuristic fallback (2), got %d", got)
	}
}

func TestKoboldExtras_CountMessages(t *testing.T) {
	f := newFakeKobold()
	defer f.Close()
	f.tokencountValue = 100

	k := NewKoboldExtras(f.srv.URL, quietLogger())
	if err := k.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}

	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system prompt"},
		{Role: openai.ChatMessageRoleUser, Content: "hello"},
		{
			Role: openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{{
				Function: openai.FunctionCall{
					Name: "f", Arguments: `{"x":1}`,
				},
			}},
		},
	}
	got := k.CountMessages(context.Background(), msgs)
	want := 100 + 3*koboldChatTemplateOverhead
	if got != want {
		t.Errorf("CountMessages = %d, want %d (100 token + 3*overhead)", got, want)
	}
	if calls := f.tokencountCalls.Load(); calls != 1 {
		t.Errorf("expected 1 batched tokencount call, got %d", calls)
	}
}

func TestKoboldExtras_CountMessages_EmptyAndFallback(t *testing.T) {
	f := newFakeKobold()
	defer f.Close()
	f.failTokencount = true

	// Detected: empty message log returns 0 without network.
	k := NewKoboldExtras(f.srv.URL, quietLogger())
	if err := k.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := k.CountMessages(context.Background(), nil); got != 0 {
		t.Errorf("nil messages = %d, want 0", got)
	}

	// Detected with failing tokencount: falls back to heuristic.
	msgs := []openai.ChatCompletionMessage{{Content: strings.Repeat("a", 400)}}
	heuristic := EstimateMessagesTokens(msgs)
	if got := k.CountMessages(context.Background(), msgs); got != heuristic {
		t.Errorf("expected fallback to heuristic %d, got %d", heuristic, got)
	}
}

func TestKoboldExtras_CountMessages_NotDetected(t *testing.T) {
	k := NewKoboldExtras("http://nonexistent.invalid", quietLogger())
	msgs := []openai.ChatCompletionMessage{{Content: "abcd"}}
	heuristic := EstimateMessagesTokens(msgs)
	if got := k.CountMessages(context.Background(), msgs); got != heuristic {
		t.Errorf("undetected client should use heuristic %d, got %d", heuristic, got)
	}
}

func TestKoboldExtras_Abort(t *testing.T) {
	f := newFakeKobold()
	defer f.Close()

	k := NewKoboldExtras(f.srv.URL, quietLogger())
	if err := k.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	k.Abort(context.Background())
	if got := f.abortCalls.Load(); got != 1 {
		t.Errorf("expected 1 abort call, got %d", got)
	}
}

func TestKoboldExtras_Abort_NotDetected_NoOp(t *testing.T) {
	f := newFakeKobold()
	defer f.Close()

	k := NewKoboldExtras(f.srv.URL, quietLogger())
	// Don't call Detect - Abort should be a silent no-op.
	k.Abort(context.Background())
	if got := f.abortCalls.Load(); got != 0 {
		t.Errorf("undetected Abort should not hit the network; got %d calls", got)
	}
}

func TestKoboldExtras_Perf(t *testing.T) {
	f := newFakeKobold()
	defer f.Close()

	k := NewKoboldExtras(f.srv.URL, quietLogger())
	if err := k.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	perf, err := k.Perf(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if perf == nil {
		t.Fatal("perf should be non-nil when detected")
	}
	if perf.LastTokenCount != 150 {
		t.Errorf("LastTokenCount = %d, want 150", perf.LastTokenCount)
	}
	if perf.TotalGens != 17 {
		t.Errorf("TotalGens = %d, want 17", perf.TotalGens)
	}
	if perf.Idle != 1 {
		t.Errorf("Idle = %d, want 1", perf.Idle)
	}
	if perf.Uptime != 7200 {
		t.Errorf("Uptime = %d, want 7200", perf.Uptime)
	}
}

func TestKoboldExtras_Perf_NotDetected(t *testing.T) {
	k := NewKoboldExtras("http://nonexistent.invalid", quietLogger())
	perf, err := k.Perf(context.Background())
	if err != nil {
		t.Errorf("undetected Perf should not error, got %v", err)
	}
	if perf != nil {
		t.Errorf("undetected Perf should return nil, got %+v", perf)
	}
}

func TestKoboldExtras_NilSafe(t *testing.T) {
	// Both Detected() and Version() must tolerate a nil receiver so the
	// agent can use them without explicit nil checks at every site.
	var k *KoboldExtras
	if k.Detected() {
		t.Error("nil Detected() should be false")
	}
	if k.Version() != "" {
		t.Error("nil Version() should be empty")
	}
}

func TestStopReasonString(t *testing.T) {
	tests := map[int]string{
		-1: "invalid",
		0:  "max-tokens-hit",
		1:  "eos-token",
		2:  "custom-stopper",
		99: "unknown(99)",
	}
	for code, want := range tests {
		if got := stopReasonString(code); got != want {
			t.Errorf("stopReasonString(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestKoboldExtras_BaseURLNormalization(t *testing.T) {
	// All of these URL forms must produce the same usable base URL.
	cases := []string{
		"http://example.com:5001",
		"http://example.com:5001/",
		"http://example.com:5001/v1",
		"http://example.com:5001/v1/",
	}
	for _, in := range cases {
		k := NewKoboldExtras(in, quietLogger())
		if k.baseURL != "http://example.com:5001" {
			t.Errorf("input %q -> baseURL %q, want http://example.com:5001", in, k.baseURL)
		}
	}
}

func TestKoboldExtras_TokencountCancelable(t *testing.T) {
	// Ensure a slow tokencount respects ctx deadlines and falls back.
	f := newFakeKobold()
	defer f.Close()
	f.tokencountDelay = 200 * time.Millisecond

	k := NewKoboldExtras(f.srv.URL, quietLogger())
	if err := k.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	// "abcdefgh" -> heuristic 2 tokens; the tokencount call will time out
	// before the fake responds, so we expect the fallback.
	if got := k.CountString(ctx, "abcdefgh"); got != 2 {
		t.Errorf("expected heuristic fallback after timeout, got %d", got)
	}
}
