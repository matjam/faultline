package adminhttp

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matjam/faultline/internal/config"
)

func TestBuildConfigForm_CoversEverySection(t *testing.T) {
	cfg := config.Default()
	form := BuildConfigForm(cfg)

	wantSections := []string{
		"api", "agent", "telegram", "log", "sandbox", "email",
		"limits", "update", "mcp", "oauth", "embeddings", "skills",
		"subagent", "admin",
	}
	got := form.SectionPaths()
	if !reflect.DeepEqual(got, wantSections) {
		t.Fatalf("section paths = %v\nwant: %v", got, wantSections)
	}
}

func TestBuildConfigForm_ClassifiesFieldsByType(t *testing.T) {
	cfg := config.Default()
	form := BuildConfigForm(cfg)

	type spec struct {
		path string
		kind ConfigKind
	}
	cases := []spec{
		// bools
		{"api.kobold_extras", KindBool},
		{"sandbox.enabled", KindBool},
		{"update.enabled", KindBool},
		{"admin.enabled", KindBool},

		// ints
		{"agent.max_tokens", KindInt},
		{"limits.recent_memory_chars", KindInt},
		{"telegram.chat_id", KindInt},

		// floats
		{"agent.temperature", KindFloat},
		{"agent.top_p", KindFloat},

		// strings
		{"api.url", KindString},
		{"agent.memory_dir", KindString},

		// secrets
		{"api.key", KindSecret},
		{"telegram.token", KindSecret},
		{"email.password", KindSecret},
		{"embeddings.api_key", KindSecret},

		// durations
		{"agent.max_sleep", KindDuration},
		{"sandbox.timeout", KindDuration},
		{"update.check_interval", KindDuration},
		{"oauth.state_ttl", KindDuration},
		{"admin.session_ttl", KindDuration},

		// selects
		{"log.level", KindSelect},
		{"update.restart_mode", KindSelect},
	}

	byPath := map[string]ConfigField{}
	for _, sec := range form.Sections {
		for _, f := range sec.Fields {
			byPath[f.Path] = f
		}
	}

	for _, c := range cases {
		got, ok := byPath[c.path]
		if !ok {
			t.Errorf("%s: missing from form", c.path)
			continue
		}
		if got.Kind != c.kind {
			t.Errorf("%s: kind = %s, want %s", c.path, got.Kind, c.kind)
		}
	}
}

func TestBuildConfigForm_DurationFormatting(t *testing.T) {
	cfg := config.Default()
	// MaxSleep default is 15m — confirm the renderer surfaces it
	// in the standard time.Duration format the input pattern
	// accepts.
	form := BuildConfigForm(cfg)
	for _, sec := range form.Sections {
		for _, f := range sec.Fields {
			if f.Path != "agent.max_sleep" {
				continue
			}
			if f.StringValue != "15m0s" && f.StringValue != "15m" {
				t.Fatalf("max_sleep formatted %q, want 15m0s", f.StringValue)
			}
			return
		}
	}
	t.Fatal("agent.max_sleep field not found")
}

func TestBuildConfigForm_SecretMasking(t *testing.T) {
	cfg := config.Default()
	cfg.API.Key = "sk-totally-secret"
	cfg.Telegram.Token = ""

	form := BuildConfigForm(cfg)
	var apiKey, tgToken ConfigField
	for _, sec := range form.Sections {
		for _, f := range sec.Fields {
			switch f.Path {
			case "api.key":
				apiKey = f
			case "telegram.token":
				tgToken = f
			}
		}
	}
	if apiKey.Kind != KindSecret || apiKey.StringValue != "" {
		t.Errorf("api.key: %+v", apiKey)
	}
	if !apiKey.HasSecret {
		t.Errorf("api.key HasSecret should be true")
	}
	if tgToken.HasSecret {
		t.Errorf("telegram.token HasSecret should be false (value is empty)")
	}
}

func TestBuildConfigForm_SubagentProfilesRowed(t *testing.T) {
	cfg := config.Default()
	cfg.Subagent.Profiles = []config.SubagentProfile{
		{Name: "fast", APIURL: "http://x/v1", Model: "qwen-7b", Purpose: "quick"},
		{Name: "deep", APIURL: "http://y/v1", Model: "gpt-4o", Purpose: "deep"},
	}

	form := BuildConfigForm(cfg)
	var sa ConfigSection
	for _, s := range form.Sections {
		if s.Path == "subagent" {
			sa = s
			break
		}
	}
	if len(sa.ProfileRows) != 2 {
		t.Fatalf("ProfileRows = %d, want 2", len(sa.ProfileRows))
	}
	// First row should expose name + api_url + model + purpose +
	// sampler fields.
	paths := map[string]bool{}
	for _, f := range sa.ProfileRows[0].Fields {
		paths[f.Path] = true
	}
	for _, want := range []string{
		"subagent.profiles.0.name",
		"subagent.profiles.0.api_url",
		"subagent.profiles.0.model",
		"subagent.profiles.0.purpose",
	} {
		if !paths[want] {
			t.Errorf("missing profile field %s; have %v", want, paths)
		}
	}
}

func TestApplyConfigForm_RoundTripScalars(t *testing.T) {
	cfg := config.Default()

	values := url.Values{}
	values.Set("api.url", "http://override.example/v1")
	values.Set("api.model", "qwen-72b")
	values.Set("agent.max_tokens", "9999")
	values.Set("agent.temperature", "0.42")
	values.Set("agent.max_sleep", "5m")
	values.Set("log.level", "debug")
	values.Set("update.restart_mode", "self-exec")

	// Bool: present with value=true, and one explicitly off.
	values.Set("sandbox.enabled__present", "1")
	values.Set("sandbox.enabled", "true")
	values.Set("update.enabled__present", "1")
	// "off" form: present marker, no value key

	warns := ApplyConfigForm(cfg, values)
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}

	if cfg.API.URL != "http://override.example/v1" {
		t.Errorf("api.url not applied: %q", cfg.API.URL)
	}
	if cfg.API.Model != "qwen-72b" {
		t.Errorf("api.model not applied: %q", cfg.API.Model)
	}
	if cfg.Agent.MaxTokens != 9999 {
		t.Errorf("agent.max_tokens not applied: %d", cfg.Agent.MaxTokens)
	}
	if cfg.Agent.Temperature != 0.42 {
		t.Errorf("agent.temperature not applied: %v", cfg.Agent.Temperature)
	}
	if time.Duration(cfg.Agent.MaxSleep) != 5*time.Minute {
		t.Errorf("agent.max_sleep not applied: %v", time.Duration(cfg.Agent.MaxSleep))
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level not applied: %q", cfg.Log.Level)
	}
	if cfg.Update.RestartMode != "self-exec" {
		t.Errorf("update.restart_mode not applied: %q", cfg.Update.RestartMode)
	}
	if !cfg.Sandbox.Enabled {
		t.Errorf("sandbox.enabled should be true")
	}
	if cfg.Update.Enabled {
		t.Errorf("update.enabled should be false (present marker, no value)")
	}
}

func TestApplyConfigForm_SecretsBlankPreserves(t *testing.T) {
	cfg := config.Default()
	cfg.API.Key = "old-secret-key"

	values := url.Values{}
	values.Set("api.key", "")

	warns := ApplyConfigForm(cfg, values)
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	if cfg.API.Key != "old-secret-key" {
		t.Errorf("blank secret should preserve existing; got %q", cfg.API.Key)
	}

	values.Set("api.key", "new-key")
	ApplyConfigForm(cfg, values)
	if cfg.API.Key != "new-key" {
		t.Errorf("non-empty secret should overwrite; got %q", cfg.API.Key)
	}
}

func TestApplyConfigForm_BadValuesProduceWarnings(t *testing.T) {
	cfg := config.Default()
	values := url.Values{}
	values.Set("agent.max_tokens", "not-a-number")
	values.Set("agent.max_sleep", "not-a-duration")

	warns := ApplyConfigForm(cfg, values)
	if len(warns) != 2 {
		t.Fatalf("warnings = %v, want 2", warns)
	}
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, "agent.max_tokens") {
		t.Errorf("missing max_tokens warning: %v", warns)
	}
	if !strings.Contains(joined, "agent.max_sleep") {
		t.Errorf("missing max_sleep warning: %v", warns)
	}
}

func TestApplyConfigForm_ProfilesEdited(t *testing.T) {
	cfg := config.Default()
	cfg.Subagent.Profiles = []config.SubagentProfile{
		{Name: "fast", APIURL: "http://x/v1", Model: "qwen-7b"},
		{Name: "deep", APIURL: "http://y/v1", Model: "gpt-4o"},
	}

	values := url.Values{}
	values.Set("subagent.profiles.0.model", "qwen-32b")
	values.Set("subagent.profiles.1.purpose", "really deep research")

	warns := ApplyConfigForm(cfg, values)
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	if cfg.Subagent.Profiles[0].Model != "qwen-32b" {
		t.Errorf("profile 0 model not updated: %q", cfg.Subagent.Profiles[0].Model)
	}
	if cfg.Subagent.Profiles[1].Purpose != "really deep research" {
		t.Errorf("profile 1 purpose not updated: %q", cfg.Subagent.Profiles[1].Purpose)
	}
}
