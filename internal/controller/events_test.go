package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

// eventHasReason reports whether one of FakeRecorder's rendered event strings
// carries exactly the given reason.
//
// FakeRecorder renders an event as "<type> <reason> <note>", so the reason is
// the second space-separated field, and it is compared as a field rather than
// as a substring of the whole line. The difference is not theoretical: a
// substring match accepts a note that merely mentions the reason in prose, and
// it accepts a *longer* reason that contains the wanted one -- so a reason
// mutated from "ServerRetiring" to "ServerRetiringMUTATED" slides straight past
// a test that was written to pin it. Two helpers in this package matched that
// way until this was fixed, and the mutation above passed against both.
//
// Note what this deliberately cannot do: the action the events API also takes
// is dropped by FakeRecorder entirely (client-go's tools/events/fake.go emits
// eventtype, reason and note and nothing else), so no assertion over these
// strings can say anything about it. That gap is closed by
// TestEveryEventfCallSitePassesAKnownAction and
// TestTheRealAPIServerRefusesAnEventWithNoAction below, not here.
func eventHasReason(rendered, reason string) bool {
	fields := strings.SplitN(rendered, " ", 3)
	return len(fields) >= 2 && fields[1] == reason
}

// TestEventNoteLeavesAShortNoteAlone is the half that protects the other 18
// call sites. eventNote sits in front of text this operator did not write, so
// the overwhelmingly common case is a note well inside the limit -- and if the
// helper reworded, re-wrapped or re-encoded those, every event assertion in
// this package would be reading something other than what the controller said.
func TestEventNoteLeavesAShortNoteAlone(t *testing.T) {
	for _, note := range []string{
		"",
		"created pod lobby-x7k2",
		"the forwarding secret changed; roll the server groups first — see the runbook",
		strings.Repeat("x", maxEventNote),
	} {
		if got := eventNote("%s", note); got != note {
			t.Errorf("eventNote(%q) = %q, want it returned byte for byte", note, got)
		}
	}
}

// TestEventNoteTruncatesAndSaysSo pins both halves of the contract: the result
// fits, and it admits to being cut. A silently shortened API server message is
// worse than a long one, because a reader cannot tell a sentence the API server
// ended from one this operator cut.
func TestEventNoteTruncatesAndSaysSo(t *testing.T) {
	// A stand-in for a PodSecurity violation list, which is the real case:
	// long, and worth reading precisely because the remedy is in its wording.
	long := "the API server refused a proxy pod: " + strings.Repeat("violates PodSecurity restricted:v1; ", 60)
	got := eventNote("%s", long)

	if len(got) > maxEventNote {
		t.Errorf("len = %d, want <= %d — this is the event the API server drops", len(got), maxEventNote)
	}
	if !strings.HasSuffix(got, eventNoteTruncated) {
		t.Errorf("got = %q, want it to end with the truncation marker %q", got, eventNoteTruncated)
	}
	if !strings.Contains(got, "status conditions") {
		t.Errorf("got = %q, want the marker to point at where the full text is", got)
	}
	// The head must still be the API server's own words, unaltered.
	if !strings.HasPrefix(got, "the API server refused a proxy pod: violates PodSecurity") {
		t.Errorf("got = %q, want the surviving head to be the original text", got)
	}
}

// TestEventNoteCutsOnARuneBoundary guards the byte/character trap. The limit is
// counted in bytes, so a note of multi-byte runes hits it at well under 1024
// characters -- and a cut that lands mid-rune produces invalid UTF-8, trading a
// too-long event for a differently-invalid one.
func TestEventNoteCutsOnARuneBoundary(t *testing.T) {
	// Em-dashes are 3 bytes each, so this is 1500 bytes of 500 characters.
	got := eventNote("%s", strings.Repeat("—", 500))
	if len(got) > maxEventNote {
		t.Errorf("len = %d bytes, want <= %d", len(got), maxEventNote)
	}
	if !utf8.ValidString(got) {
		t.Errorf("got = %q, want valid UTF-8 — the cut split a rune", got)
	}
}

// TestTheRealAPIServerAcceptsATruncatedNoteAndRefusesAnUntruncatedOne is the
// assertion that would actually have caught this regression.
//
// Every other event assertion in this package reads FakeRecorder, which
// validates nothing and would accept a note of any length -- which is exactly
// why the migration onto events.k8s.io/v1 could introduce a dropped-event bug
// that the whole suite stayed green through. This one goes to envtest's real
// API server, so it fails if the limit moves, if eventNote stops enforcing it,
// or if the note ever again reaches Eventf unbounded.
func TestTheRealAPIServerAcceptsATruncatedNoteAndRefusesAnUntruncatedOne(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	create := func(name, note string) error {
		return c.Create(ctx, &eventsv1.Event{
			ObjectMeta:          metav1.ObjectMeta{Name: name, Namespace: ns},
			EventTime:           metav1.NowMicro(),
			ReportingController: "spawnery.cloud/proxygroup",
			ReportingInstance:   "spawnery-operator-0",
			Action:              actionCreateProxyPod,
			Reason:              "ProxyPodBlocked",
			Type:                corev1.EventTypeWarning,
			Regarding: corev1.ObjectReference{
				Kind: "Namespace", Name: ns, Namespace: ns, APIVersion: "v1",
			},
			Note: note,
		})
	}

	// The note the refused-proxy-pod site would build from a long admission
	// refusal, before eventNote sees it.
	raw := "the API server refused a proxy pod: " +
		strings.Repeat("violates PodSecurity restricted:v1; ", 60)
	if len(raw) <= maxEventNote {
		t.Fatalf("the fixture is %d bytes, which is not over the limit it is meant to exceed", len(raw))
	}

	if err := create("untruncated", raw); err == nil {
		t.Error("the API server accepted a note over the limit; " +
			"if the limit has been lifted, eventNote and its comment need revisiting")
	} else if !strings.Contains(err.Error(), "note") && !strings.Contains(err.Error(), "message") {
		t.Errorf("refused for the wrong reason: %v", err)
	}

	if err := create("truncated", eventNote("%s", raw)); err != nil {
		t.Errorf("the API server refused an eventNote-truncated note: %v — "+
			"this is the event an operator loses", err)
	}
}

// TestAnUnschedulableProxyPodsEventStaysWithinTheNoteLimit is the wiring test.
//
// The three tests above prove eventNote is correct; none of them proves any
// controller calls it, and a correct helper nobody reaches would leave the
// regression exactly where it was. This drives a real call site --
// reportBlockedProxies, whose note carries the scheduler's own explanation, one
// of the five the review named -- with a message far over the limit, and reads
// what the recorder was actually handed.
func TestAnUnschedulableProxyPodsEventStaysWithinTheNoteLimit(t *testing.T) {
	rec := events.NewFakeRecorder(10)
	r := &ProxyGroupReconciler{Recorder: rec}
	group := &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "spawnery-test"},
	}
	// What a scheduler says on a large cluster: one clause per node pool.
	scheduler := strings.Repeat("0/240 nodes are available: 240 node(s) didn't have free ports; ", 40)
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-proxy-abcde", Namespace: "spawnery-test"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  corev1.ConditionFalse,
			Reason:  "Unschedulable",
			Message: scheduler,
		}}},
	}}

	r.reportBlockedProxies(group, pods)

	var got string
	select {
	case got = <-rec.Events:
	default:
		t.Fatal("no event was recorded")
	}
	// FakeRecorder emits "<type> <reason> <note>", so the note is what follows
	// the second space -- the part the API server length-checks.
	prefix := "Warning ProxyPodBlocked "
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("event = %q, want it to start %q", got, prefix)
	}
	note := strings.TrimPrefix(got, prefix)
	if len(note) > maxEventNote {
		t.Errorf("note = %d bytes, want <= %d — the API server drops this event and "+
			"the operator never learns why the pod is stuck", len(note), maxEventNote)
	}
	if !strings.HasSuffix(note, eventNoteTruncated) {
		t.Errorf("note = %q, want the truncation marker", note)
	}
	// The condition keeps the whole thing; that is what the marker points at.
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if cond == nil || !strings.Contains(cond.Message, "0/240 nodes are available") {
		t.Fatalf("Degraded = %+v, want the full scheduler text the event points at", cond)
	}
	if len(cond.Message) <= maxEventNote {
		t.Errorf("the condition is %d bytes, so this test is not exercising truncation", len(cond.Message))
	}
}

// knownActions is every action constant in events.go, keyed by the identifier
// the call sites spell. The map is written out rather than derived so that both
// halves are checked: the value side is the constant itself, so a constant that
// loses its value fails to compile into a non-empty string here, and the key
// side is the name TestEveryEventfCallSitePassesAKnownAction looks for in the
// source.
var knownActions = map[string]string{
	"actionAdoptPod":       actionAdoptPod,
	"actionCreatePod":      actionCreatePod,
	"actionDeletePod":      actionDeletePod,
	"actionCreateProxyPod": actionCreateProxyPod,
	"actionDrainProxy":     actionDrainProxy,
	"actionCreateServer":   actionCreateServer,
	"actionDeleteServer":   actionDeleteServer,
	"actionRetireServer":   actionRetireServer,

	"actionBootstrapNamespace": actionBootstrapNamespace,

	"actionSyncStatus": actionSyncStatus,
}

// wantEventfSites is how many Eventf call sites this package has.
//
// Asserted rather than logged, because a count that is only printed cannot
// notice its own coverage shrinking. Deleting a call site outright left the
// scan green at 22 when this was a t.Logf, and moving a controller into a
// subpackage would have done the same silently. Changing this number is
// therefore a deliberate act with a diff, which is what it should be: adding
// an event is a change to the operator's output.
const wantEventfSites = 25

// TestEveryEventfCallSitePassesAKnownAction reads this package's own source.
//
// It is here because nothing else in the repository can see the action at all.
// events.FakeRecorder renders an event as eventtype, reason and note and drops
// the action entirely (client-go v0.36.0, tools/events/fake.go), so every
// event assertion in this package is blind to it; envtest's tests go through
// the same fake; and go vet cannot help either, because it cannot see through
// the events.EventRecorder interface to know Eventf's note is a format string
// -- a missing argument at one of these call sites produces no diagnostic. The
// four action constants were replaced with garbage during milestone 6e's final
// review and the whole package stayed green.
//
// What is at stake is not cosmetic. events.k8s.io/v1 rejects an event with an
// empty action outright (TestTheRealAPIServerRefusesAnEventWithNoAction below
// measures that against a real API server), the broadcaster classifies the
// refusal as non-retryable and abandons the event with a klog line, and
// nothing on the reconciled object says an event was lost. A new call site
// that passes "" would go green through unit tests, envtest and e2e alike.
//
// A source-level check rather than twenty-four assertions: what needs
// guarding is a property of every call site, not the particular string any one
// of them passes, and a table restating twenty-four literals would be a
// second copy of the code that goes stale the first time somebody adds a
// twenty-fifth. This reads whatever is there.
//
// Three assertions, and the second and third exist because the first alone was
// weaker than it looked. It requires every Eventf's action argument to be an
// identifier named in knownActions. It requires the number of call sites found
// to be wantEventfSites, because a count that is only logged cannot notice its
// own corpus shrinking -- a deleted call site left it green at 22. And it
// requires that no local anywhere in the package shadows one of those ten
// names, which is what makes matching by name mean anything at all: without
// it, `actionCreatePod := ""` above a call site passes. See shadowedActions.
//
// What none of the three reaches, said here rather than left to be assumed: it
// cannot tell whether the constant a call site chose is the right one for that
// call site. actionSyncStatus where actionCreatePod was meant passes.
func TestEveryEventfCallSitePassesAKnownAction(t *testing.T) {
	for name, value := range knownActions {
		if value == "" {
			t.Errorf("%s is empty; events.k8s.io/v1 refuses an event with no action", name)
		}
	}

	fset := token.NewFileSet()
	var files []*ast.File
	// Recursive, not one directory deep. A non-recursive scan makes this
	// test's coverage a function of where the controllers happen to live: move
	// one into a subpackage of internal/controller and its call sites leave
	// the corpus with nothing saying so.
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata is not compiled and may hold anything.
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		// Test files are excluded on purpose: a fixture may legitimately drive
		// a recorder with a literal, and what this guards is the operator's
		// own output.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		files = append(files, file)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the package sources: %v", err)
	}

	sites := 0
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fun, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || fun.Sel.Name != "Eventf" {
				return true
			}
			sites++
			pos := fset.Position(call.Pos())
			// Eventf(regarding, related, eventtype, reason, action, note, ...).
			const actionArg = 4
			if len(call.Args) <= actionArg {
				t.Errorf("%s: Eventf with %d arguments, too few to carry an action",
					pos, len(call.Args))
				return true
			}
			ident, ok := call.Args[actionArg].(*ast.Ident)
			if !ok {
				t.Errorf("%s: the action argument is not one of events.go's action "+
					"constants but %T; add it to events.go and to knownActions, or use "+
					"one that is already there", pos, call.Args[actionArg])
				return true
			}
			if _, known := knownActions[ident.Name]; !known {
				t.Errorf("%s: action %q is not one of events.go's action constants. "+
					"events.go's own rule is that adding an event means choosing from "+
					"that list, not inventing a string", pos, ident.Name)
			}
			return true
		})
	}

	if sites != wantEventfSites {
		t.Errorf("found %d Eventf call sites, want %d. If an event was added or "+
			"removed on purpose, change wantEventfSites in the same commit; if it "+
			"was not, a call site has moved out of this scan's reach",
			sites, wantEventfSites)
	}

	// The other half of matching by name, and the reason matching by name is
	// sound at all. See shadowedActions.
	for _, file := range files {
		for _, shadow := range shadowedActions(fset, file) {
			t.Errorf("%s: %s shadows one of events.go's action constants. "+
				"TestEveryEventfCallSitePassesAKnownAction matches the action "+
				"argument by identifier name and does not resolve types, so a "+
				"local of this name would let a call site pass while carrying "+
				"something else entirely -- rename the local", shadow.pos, shadow.name)
		}
	}
}

// shadow is one local declaration that takes an action constant's name.
type shadow struct {
	name string
	pos  token.Position
}

// shadowedActions finds locals that shadow one of events.go's action
// constants.
//
// This is what makes the name-matching in the test above sound. That test asks
// whether the action argument is an identifier called, say, actionCreatePod;
// it does not and cannot resolve what that identifier refers to, because
// resolving it means running a type checker over the package, which is a great
// deal of machinery to answer one question. So the identifier's meaning is
// pinned from the other side instead: the ten names are package-level
// constants, nothing in the package has any business declaring a local of the
// same name, and if nothing does then the name determines the constant.
// `actionCreatePod := ""` above a call site went green before this existed --
// which is exactly the shape the test claims to catch.
//
// The remaining limit, stated rather than papered over: none of this says the
// constant a call site chose is the *right* one for that call site.
// actionSyncStatus where actionCreatePod was meant passes both halves, and
// only a reader applying events.go's own rule can tell. docs/known-issues.md
// says the same.
//
// Declarations, not uses: a parameter, a `:=`, a `var`/`const` inside a
// function, a range variable, a function literal's parameter. A package-level
// redeclaration is not checked because the compiler refuses it.
func shadowedActions(fset *token.FileSet, file *ast.File) []shadow {
	var found []shadow
	report := func(idents ...*ast.Ident) {
		for _, id := range idents {
			if id == nil || id.Name == "_" {
				continue
			}
			if _, known := knownActions[id.Name]; known {
				found = append(found, shadow{name: id.Name, pos: fset.Position(id.Pos())})
			}
		}
	}
	fields := func(list *ast.FieldList) {
		if list == nil {
			return
		}
		for _, f := range list.List {
			report(f.Names...)
		}
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			// A package-level var or const of one of these names is a
			// redeclaration the compiler rejects, so there is nothing here to
			// find.
			continue
		}
		fields(fn.Recv)
		ast.Inspect(fn, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncType:
				fields(node.Params)
				fields(node.Results)
			case *ast.AssignStmt:
				if node.Tok == token.DEFINE {
					for _, lhs := range node.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							report(id)
						}
					}
				}
			case *ast.RangeStmt:
				if node.Tok == token.DEFINE {
					if id, ok := node.Key.(*ast.Ident); ok {
						report(id)
					}
					if id, ok := node.Value.(*ast.Ident); ok {
						report(id)
					}
				}
			case *ast.ValueSpec:
				report(node.Names...)
			}
			return true
		})
	}
	return found
}

// TestTheRealAPIServerRefusesAnEventWithNoAction measures the premise the test
// above rests on, against envtest's real API server rather than a comment.
//
// If events.k8s.io/v1 ever stopped refusing an empty action, the source-level
// check would still be worth having for legibility, but it would no longer be
// guarding a dropped event -- and a reader deserves to be told which of those
// two things it is. This is what tells them.
func TestTheRealAPIServerRefusesAnEventWithNoAction(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	create := func(name, action string) error {
		return c.Create(ctx, &eventsv1.Event{
			ObjectMeta:          metav1.ObjectMeta{Name: name, Namespace: ns},
			EventTime:           metav1.NowMicro(),
			ReportingController: "spawnery.cloud/proxygroup",
			ReportingInstance:   "spawnery-operator-0",
			Action:              action,
			Reason:              "ProxyPodBlocked",
			Type:                corev1.EventTypeWarning,
			Regarding: corev1.ObjectReference{
				Kind: "Namespace", Name: ns, Namespace: ns, APIVersion: "v1",
			},
			Note: "a proxy pod could not be scheduled",
		})
	}

	if err := create("no-action", ""); err == nil {
		t.Error("the API server accepted an event with an empty action; the whole " +
			"reason every call site carries one has moved, and events.go's " +
			"opening comment needs revisiting")
	} else if !strings.Contains(err.Error(), "action") {
		t.Errorf("refused for the wrong reason: %v", err)
	}

	if err := create("with-action", actionSyncStatus); err != nil {
		t.Errorf("the API server refused an event carrying %q: %v",
			actionSyncStatus, err)
	}
}
