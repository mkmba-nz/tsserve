package lambda

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	tagging "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	taggingtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"

	"mkmba.nz/tsserve/ecs"
)

const (
	arnA = "arn:aws:lambda:ap-southeast-2:111122223333:function:alpha"
	arnB = "arn:aws:lambda:ap-southeast-2:111122223333:function:bravo"
)

// fakeTagging serves pages of tagged functions. Every page but the last
// carries a token; the last carries an empty one, as the real API does, or
// none when nilLastToken is set.
type fakeTagging struct {
	pages        [][]taggingtypes.ResourceTagMapping
	nilLastToken bool
	err          error
	calls        []*tagging.GetResourcesInput
}

func (f *fakeTagging) GetResources(_ context.Context, in *tagging.GetResourcesInput, _ ...func(*tagging.Options)) (*tagging.GetResourcesOutput, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	page := 0
	if tok := aws.ToString(in.PaginationToken); tok != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(tok, "page-"))
		if err != nil {
			return nil, fmt.Errorf("bad token %q", tok)
		}
		page = n
	}
	out := &tagging.GetResourcesOutput{}
	if !f.nilLastToken {
		out.PaginationToken = aws.String("")
	}
	if page < len(f.pages) {
		out.ResourceTagMappingList = f.pages[page]
	}
	if page+1 < len(f.pages) {
		out.PaginationToken = aws.String("page-" + strconv.Itoa(page+1))
	}
	return out, nil
}

// serve replaces the served functions with a single page.
func (f *fakeTagging) serve(mappings ...taggingtypes.ResourceTagMapping) {
	f.pages = [][]taggingtypes.ResourceTagMapping{mappings}
}

func fn(arn string, tags map[string]string) taggingtypes.ResourceTagMapping {
	m := taggingtypes.ResourceTagMapping{ResourceARN: aws.String(arn)}
	for k, v := range tags {
		m.Tags = append(m.Tags, taggingtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return m
}

type recordingReg struct {
	registered   map[string]Function
	registers    []string
	deregistered []string
	events       []string // "register <key>" and "deregister <key>", in call order
	failWith     error
}

func newRecordingReg() *recordingReg {
	return &recordingReg{registered: map[string]Function{}}
}

func (r *recordingReg) RegisterFunction(key string, f *Function) error {
	r.registers = append(r.registers, key)
	r.events = append(r.events, "register "+key)
	if r.failWith != nil {
		return r.failWith
	}
	r.registered[key] = *f
	return nil
}

func (r *recordingReg) Deregister(key string) {
	r.deregistered = append(r.deregistered, key)
	r.events = append(r.events, "deregister "+key)
	delete(r.registered, key)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestWatcher(api TaggingAPI, reg Registrar, logger *slog.Logger) *Watcher {
	return NewWatcher(Config{Account: "test"}, api, reg, logger)
}

func mustCycle(t *testing.T, w *Watcher) {
	t.Helper()
	if err := w.cycle(context.Background()); err != nil {
		t.Fatalf("cycle: %v", err)
	}
}

func TestCycle_RegistersEachTaggedFunction(t *testing.T) {
	api := &fakeTagging{}
	api.serve(
		fn(arnA, map[string]string{
			"tsserve.enable":    "true",
			"tsserve.service":   "svc:alpha",
			"tsserve.caps":      "example.com/cap/one, example.com/cap/two",
			"tsserve.qualifier": "live",
			// Ignored for a function.
			"tsserve.port":   "8080",
			"tsserve.scheme": "https",
		}),
		fn(arnB, map[string]string{
			"tsserve.enable":  "true",
			"tsserve.service": "svc:bravo",
		}),
	)
	reg := newRecordingReg()
	w := newTestWatcher(api, reg, discardLogger())

	mustCycle(t, w)

	want := map[string]Function{
		arnA: {Service: "svc:alpha", Caps: []string{"example.com/cap/one", "example.com/cap/two"}, ARN: arnA, Qualifier: "live"},
		arnB: {Service: "svc:bravo", ARN: arnB},
	}
	if len(reg.registered) != len(want) {
		t.Fatalf("registered %d functions, want %d: %+v", len(reg.registered), len(want), reg.registered)
	}
	for key, w := range want {
		got := reg.registered[key]
		if got.Service != w.Service || got.ARN != w.ARN || got.Qualifier != w.Qualifier || !slices.Equal(got.Caps, w.Caps) {
			t.Errorf("registered[%s] = %+v, want %+v", key, got, w)
		}
	}

	if len(api.calls) != 1 {
		t.Fatalf("GetResources calls = %d, want 1", len(api.calls))
	}
	in := api.calls[0]
	if !slices.Equal(in.ResourceTypeFilters, []string{"lambda:function"}) {
		t.Errorf("ResourceTypeFilters = %v, want [lambda:function]", in.ResourceTypeFilters)
	}
	if len(in.TagFilters) != 1 || aws.ToString(in.TagFilters[0].Key) != "tsserve.enable" ||
		!slices.Equal(in.TagFilters[0].Values, []string{"true"}) {
		t.Errorf("TagFilters = %+v, want one filter tsserve.enable=[true]", in.TagFilters)
	}
}

func TestCycle_RepeatCycleDoesNotReregister(t *testing.T) {
	api := &fakeTagging{}
	api.serve(fn(arnA, map[string]string{"tsserve.service": "svc:alpha"}))
	reg := newRecordingReg()
	w := newTestWatcher(api, reg, discardLogger())

	mustCycle(t, w)
	mustCycle(t, w)

	if len(reg.registers) != 1 || len(reg.deregistered) != 0 {
		t.Fatalf("registers=%v deregistered=%v, want one register and no deregister", reg.registers, reg.deregistered)
	}
}

func TestCycle_DeregistersFunctionNoLongerTagged(t *testing.T) {
	api := &fakeTagging{}
	api.serve(
		fn(arnA, map[string]string{"tsserve.service": "svc:alpha"}),
		fn(arnB, map[string]string{"tsserve.service": "svc:bravo"}),
	)
	reg := newRecordingReg()
	w := newTestWatcher(api, reg, discardLogger())
	mustCycle(t, w)

	api.serve(fn(arnB, map[string]string{"tsserve.service": "svc:bravo"}))
	mustCycle(t, w)

	if !slices.Equal(reg.deregistered, []string{arnA}) {
		t.Fatalf("deregistered = %v, want [%s]", reg.deregistered, arnA)
	}
	if _, ok := reg.registered[arnB]; !ok || len(reg.registered) != 1 {
		t.Fatalf("registered = %v, want only %s", reg.registered, arnB)
	}
}

func TestCycle_TagChangeReregisters(t *testing.T) {
	base := map[string]string{"tsserve.service": "svc:alpha", "tsserve.qualifier": "blue"}
	tests := []struct {
		name   string
		change map[string]string
		want   Function
	}{
		{
			name:   "qualifier",
			change: map[string]string{"tsserve.qualifier": "green"},
			want:   Function{Service: "svc:alpha", ARN: arnA, Qualifier: "green"},
		},
		{
			name:   "service",
			change: map[string]string{"tsserve.service": "svc:renamed"},
			want:   Function{Service: "svc:renamed", ARN: arnA, Qualifier: "blue"},
		},
		{
			name:   "caps",
			change: map[string]string{"tsserve.caps": "example.com/cap/one"},
			want:   Function{Service: "svc:alpha", ARN: arnA, Qualifier: "blue", Caps: []string{"example.com/cap/one"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeTagging{}
			api.serve(fn(arnA, base))
			reg := newRecordingReg()
			w := newTestWatcher(api, reg, discardLogger())
			mustCycle(t, w)

			changed := map[string]string{}
			for k, v := range base {
				changed[k] = v
			}
			for k, v := range tt.change {
				changed[k] = v
			}
			api.serve(fn(arnA, changed))
			mustCycle(t, w)

			want := []string{"register " + arnA, "deregister " + arnA, "register " + arnA}
			if !slices.Equal(reg.events, want) {
				t.Errorf("events = %v, want %v (deregister before the new register)", reg.events, want)
			}
			got := reg.registered[arnA]
			if got.Service != tt.want.Service || got.Qualifier != tt.want.Qualifier || !slices.Equal(got.Caps, tt.want.Caps) {
				t.Errorf("registered = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCycle_CapsReorderIsNotAChange(t *testing.T) {
	api := &fakeTagging{}
	api.serve(fn(arnA, map[string]string{"tsserve.service": "svc:alpha", "tsserve.caps": "example.com/a,example.com/b"}))
	reg := newRecordingReg()
	w := newTestWatcher(api, reg, discardLogger())
	mustCycle(t, w)

	api.serve(fn(arnA, map[string]string{"tsserve.service": "svc:alpha", "tsserve.caps": "example.com/b, example.com/a"}))
	mustCycle(t, w)

	if want := []string{"register " + arnA}; !slices.Equal(reg.events, want) {
		t.Fatalf("events = %v, want %v", reg.events, want)
	}
}

func TestCycle_QualifierForms(t *testing.T) {
	for _, q := range []string{"$LATEST", "7", "live", "blue_green-2"} {
		api := &fakeTagging{}
		api.serve(fn(arnA, map[string]string{"tsserve.service": "svc:alpha", "tsserve.qualifier": q}))
		reg := newRecordingReg()
		mustCycle(t, newTestWatcher(api, reg, discardLogger()))
		if got := reg.registered[arnA].Qualifier; got != q {
			t.Errorf("qualifier %q: registered qualifier = %q", q, got)
		}
	}
}

func TestCycle_InvalidTagsWarnOnceAndKeepServingRegistration(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	api := &fakeTagging{}
	api.serve(fn(arnA, map[string]string{"tsserve.service": "svc:alpha"}))
	reg := newRecordingReg()
	w := newTestWatcher(api, reg, logger)
	mustCycle(t, w)

	api.serve(fn(arnA, map[string]string{"tsserve.service": "alpha"}))
	mustCycle(t, w)
	mustCycle(t, w)

	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("warnings = %d, want 1; log:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "function="+arnA) || !strings.Contains(buf.String(), `must start with \"svc:\"`) {
		t.Errorf("warning does not name the function and the reason; log:\n%s", buf.String())
	}
	if len(reg.deregistered) != 0 {
		t.Fatalf("deregistered = %v, want the previous registration kept", reg.deregistered)
	}
	if got := reg.registered[arnA]; got.Service != "svc:alpha" {
		t.Fatalf("registered[%s] = %+v, want the svc:alpha registration kept", arnA, got)
	}

	// Fixed tags are picked up on the next cycle.
	api.serve(fn(arnA, map[string]string{"tsserve.service": "svc:fixed"}))
	mustCycle(t, w)
	if got := reg.registered[arnA]; got.Service != "svc:fixed" {
		t.Fatalf("after fixing tags registered[%s] = %+v, want svc:fixed", arnA, got)
	}
}

func TestCycle_InvalidTagsNeverRegistered(t *testing.T) {
	tests := []struct {
		name string
		tags map[string]string
	}{
		{name: "missing service", tags: map[string]string{"tsserve.caps": "x"}},
		{name: "service without svc prefix", tags: map[string]string{"tsserve.service": "alpha"}},
		{name: "qualifier with a space", tags: map[string]string{"tsserve.service": "svc:alpha", "tsserve.qualifier": "v 1"}},
		{name: "qualifier with a stray dollar", tags: map[string]string{"tsserve.service": "svc:alpha", "tsserve.qualifier": "a$b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeTagging{}
			api.serve(fn(arnA, tt.tags))
			reg := newRecordingReg()
			w := newTestWatcher(api, reg, discardLogger())
			mustCycle(t, w)
			mustCycle(t, w)
			if len(reg.registers) != 0 {
				t.Fatalf("registers = %v, want none", reg.registers)
			}

			// Re-examined every cycle: fixed tags register on the next one.
			api.serve(fn(arnA, map[string]string{"tsserve.service": "svc:fixed"}))
			mustCycle(t, w)
			if got := reg.registered[arnA]; got.Service != "svc:fixed" {
				t.Fatalf("after fixing tags registered[%s] = %+v, want svc:fixed", arnA, got)
			}
		})
	}
}

func TestCycle_ConsumesEveryPageBeforeDeregistering(t *testing.T) {
	api := &fakeTagging{pages: [][]taggingtypes.ResourceTagMapping{
		{fn(arnA, map[string]string{"tsserve.service": "svc:alpha"})},
		{fn(arnB, map[string]string{"tsserve.service": "svc:bravo"})},
	}}
	reg := newRecordingReg()
	w := newTestWatcher(api, reg, discardLogger())

	mustCycle(t, w)
	mustCycle(t, w)

	if len(reg.registered) != 2 {
		t.Fatalf("registered = %v, want both pages' functions", reg.registered)
	}
	if len(reg.deregistered) != 0 {
		t.Fatalf("deregistered = %v, want none", reg.deregistered)
	}
	if len(api.calls) != 4 {
		t.Fatalf("GetResources calls = %d, want 4 (two pages, two cycles)", len(api.calls))
	}
	if tok := aws.ToString(api.calls[1].PaginationToken); tok != "page-1" {
		t.Errorf("second call PaginationToken = %q, want page-1", tok)
	}
}

func TestCycle_ThreePagesEndingInAnAbsentToken(t *testing.T) {
	arnC := "arn:aws:lambda:ap-southeast-2:111122223333:function:charlie"
	api := &fakeTagging{nilLastToken: true, pages: [][]taggingtypes.ResourceTagMapping{
		{fn(arnA, map[string]string{"tsserve.service": "svc:alpha"})},
		{fn(arnB, map[string]string{"tsserve.service": "svc:bravo"})},
		{fn(arnC, map[string]string{"tsserve.service": "svc:charlie"})},
	}}
	reg := newRecordingReg()
	mustCycle(t, newTestWatcher(api, reg, discardLogger()))

	if len(reg.registered) != 3 {
		t.Fatalf("registered = %v, want all three pages' functions", reg.registered)
	}
	var tokens []string
	for _, c := range api.calls {
		tokens = append(tokens, aws.ToString(c.PaginationToken))
	}
	if want := []string{"", "page-1", "page-2"}; !slices.Equal(tokens, want) {
		t.Fatalf("tokens sent = %q, want %q", tokens, want)
	}
}

func TestCycle_FailedRegisterIsNotActiveAndIsRetried(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	api := &fakeTagging{}
	api.serve(fn(arnA, map[string]string{"tsserve.service": "svc:alpha"}))
	reg := newRecordingReg()
	reg.failWith = errors.New("listener refused")
	w := newTestWatcher(api, reg, logger)

	mustCycle(t, w)
	mustCycle(t, w)
	if len(reg.registers) != 2 {
		t.Fatalf("register attempts = %d, want 2 (retried each cycle)", len(reg.registers))
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("warnings = %d, want 1 for an unchanged failure; log:\n%s", n, buf.String())
	}

	reg.failWith = nil
	mustCycle(t, w)
	mustCycle(t, w)
	if len(reg.registers) != 3 {
		t.Fatalf("register attempts = %d, want 3 (none once active)", len(reg.registers))
	}
	if _, ok := reg.registered[arnA]; !ok {
		t.Fatalf("%s not registered after recovery", arnA)
	}
}

func TestCycle_ListFailureKeepsRegistrations(t *testing.T) {
	api := &fakeTagging{}
	api.serve(fn(arnA, map[string]string{"tsserve.service": "svc:alpha"}))
	reg := newRecordingReg()
	w := newTestWatcher(api, reg, discardLogger())
	mustCycle(t, w)

	api.err = errors.New("AccessDenied")
	if err := w.cycle(context.Background()); err == nil || !strings.Contains(err.Error(), "GetResources") {
		t.Fatalf("cycle error = %v, want a GetResources error", err)
	}
	if len(reg.deregistered) != 0 {
		t.Fatalf("deregistered = %v, want none on a failed cycle", reg.deregistered)
	}
}

func TestReportCycle_ReaderHealthAndCadence(t *testing.T) {
	registry := ecs.NewRegistry()
	reader := registry.Add(ecs.ReaderStatus{Name: "test"})
	api := &fakeTagging{err: errors.New("throttled")}
	w := NewWatcher(Config{Reader: reader}, api, newRecordingReg(), discardLogger())

	if err := w.reportCycle(context.Background()); err == nil {
		t.Fatal("reportCycle: want error")
	}
	st := registry.Snapshot()[0]
	if st.Healthy || st.LastError != "GetResources: throttled" || st.Polls != 1 {
		t.Fatalf("after failure status = %+v, want unhealthy with the error and one poll", st)
	}
	if got := w.nextInterval(); got != w.cfg.RetryInterval {
		t.Fatalf("nextInterval after failure = %s, want retry interval %s", got, w.cfg.RetryInterval)
	}

	api.err = nil
	if err := w.reportCycle(context.Background()); err != nil {
		t.Fatalf("reportCycle: %v", err)
	}
	st = registry.Snapshot()[0]
	if !st.Healthy || st.LastError != "" || st.Polls != 2 {
		t.Fatalf("after success status = %+v, want healthy with no error and two polls", st)
	}
	if got := w.nextInterval(); got != DefaultPollInterval {
		t.Fatalf("nextInterval after success = %s, want %s", got, DefaultPollInterval)
	}
}

func TestNextInterval_RetryNeverFasterThanPoll(t *testing.T) {
	w := NewWatcher(Config{PollInterval: time.Minute, RetryInterval: 10 * time.Second},
		&fakeTagging{}, newRecordingReg(), discardLogger())
	if got := w.nextInterval(); got != time.Minute {
		t.Fatalf("unhealthy nextInterval = %s, want the 1m poll interval", got)
	}
}

func TestCycle_ResolvesAccountBeforeRegistering(t *testing.T) {
	api := &fakeTagging{}
	api.serve(fn(arnA, map[string]string{"tsserve.service": "svc:alpha"}))
	reg := newRecordingReg()

	var resolved []string
	resolveErr := errors.New("role not assumable")
	cfg := Config{
		Account: "accounts[0]",
		ResolveAccount: func(context.Context) (string, error) {
			if resolveErr != nil {
				return "", resolveErr
			}
			return "111122223333", nil
		},
		OnAccountResolved: func(account string) {
			if len(reg.registers) != 0 {
				panic("OnAccountResolved ran after a registration")
			}
			resolved = append(resolved, account)
		},
	}
	w := NewWatcher(cfg, api, reg, discardLogger())

	if err := w.cycle(context.Background()); !errors.Is(err, resolveErr) {
		t.Fatalf("cycle error = %v, want the resolve error", err)
	}
	if len(api.calls) != 0 || len(reg.registers) != 0 {
		t.Fatalf("calls=%d registers=%v, want no discovery before the identity resolves", len(api.calls), reg.registers)
	}

	resolveErr = nil
	mustCycle(t, w)
	mustCycle(t, w)
	if !slices.Equal(resolved, []string{"111122223333"}) {
		t.Fatalf("OnAccountResolved calls = %v, want exactly one with the account", resolved)
	}
	if w.cfg.Account != "111122223333" {
		t.Fatalf("log identity = %q, want the resolved account", w.cfg.Account)
	}
}
