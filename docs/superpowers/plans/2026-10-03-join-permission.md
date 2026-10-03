# Join Permission Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `ServerGroup` can name a permission a player needs to join it (`Required`) or one whose explicit `false` keeps a player out (`DenyOnly`); backends refuse, proxies route around.

**Architecture:** A new optional `spec.joinPermission` travels to every agent in `GroupState` (resolved node name plus a deny-only flag). A pure `JoinRules.mayJoin` in `agent/common` decides from a three-valued permission answer. The Paper `LoginGateListener` is the binding check; the Velocity agent evaluates the same rule in the target server's LuckPerms contexts inside `Router`, `Drain`, `Rescue`, the transfer landing and `ServerPreConnectEvent`.

**Tech Stack:** Go (controller-runtime, kubebuilder markers, envtest), protobuf, Kotlin (Paper and Velocity agents, JUnit 5), LuckPerms API 5.5 (compileOnly), Nix.

**Spec:** `docs/superpowers/specs/2026-10-03-join-permission-design.md`

## Global Constraints

- Work in the worktree `/home/paul/git/spawnery-join`, branch `feat/join-permission`.
- Every build and test command runs in the dev shell: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery-join -c <cmd>`, started from the worktree root. Never `cd` somewhere else before `nix develop`. Below, `NIX` stands for `nix --extra-experimental-features 'nix-command flakes'`.
- Nix builds read the git index: `git add` every new or changed file before `NIX build .#agents`.
- Kotlin tests run only through `NIX build /home/paul/git/spawnery-join#agents --no-link -L` (all JUnit suites on JDK 25); a failing test fails the build.
- Generated files are committed: after API or `.proto` changes run `NIX develop /home/paul/git/spawnery-join -c make manifests generate proto` and commit `config/crd/bases/`, `charts/spawnery/templates/crds.yaml`, `zz_generated.deepcopy.go`, `internal/agentpb/`, `agent/common/src/proto/java/`, `docs/reference/crds.md`.
- Field numbers: `GroupState.join_permission = 11`, `GroupState.join_permission_deny_only = 12`.
- Mode enum values exactly `Required` and `DenyOnly`; default `Required`.
- Default node exactly `spawnery.join.<group name>`; node pattern `^[a-z0-9_.-]+$`, max length 128.
- Refusal translation key exactly `spawnery.join.denied`, fallback text exactly `You may not join %s.`
- The rule never enters a pod spec or `DesiredServerHash`; `internal/podspec/hash_golden_test.go` must stay untouched.
- Comments follow the user's rule: none unless a reader cannot derive it from the code beside it. Everything written is English.
- Commits are Conventional Commits with a scope (`feat(api): …`, `feat(agent): …`), body wrapped at 72 columns, ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Commits are gpg-signed; on `paul-desktop` the passphrase dialog opens as a window, just commit.

## Review Focus

1. **Every fallback group forbidden.** `Router.choose` returns null and the proxy refuses the connection; the warning must say join rules may be why, not only "no server available". Pinned in Task 6.
2. **LuckPerms installed on the proxy but not loaded.** The LuckPerms lookup answers null and the Velocity lookup decides; it must never throw out of an event handler. Pinned in Task 5.
3. **A server the directory does not know** (registered by a `configOverlay`, not by the operator). `groupOf` is null and `ServerPreConnectEvent` lets the connection through. Pinned in Task 6.
4. **An operator on Paper without LuckPerms.** Bukkit answers `hasPermission` true for an op on an unset node; `Required` must admit them and `DenyOnly` must too. Pinned in Task 4.
5. **A group without a display name.** The refusal names the group by its name, never an empty string. Pinned in Task 4.

---

### Task 1: The API field

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (new types near `ServerGroupType`, new field after `EnforcePlayableSlots` at ~line 219)
- Test: `api/v1alpha1/servergroup_types_test.go`, `api/v1alpha1/servergroup_envtest_test.go`
- Regenerate: CRDs, chart CRDs, deepcopy, `docs/reference/crds.md`

**Interfaces:**
- Produces: `type JoinPermissionMode string`, constants `JoinPermissionRequired`, `JoinPermissionDenyOnly`; `type JoinPermission struct { Node string; Mode JoinPermissionMode }`; field `ServerGroupSpec.JoinPermission *JoinPermission`; method `func (j *JoinPermission) ResolvedNode(group string) string` (nil receiver returns `""`); method `func (j *JoinPermission) DenyOnly() bool` (nil receiver returns false).

- [ ] **Step 1: Write the failing unit test** in `api/v1alpha1/servergroup_types_test.go`:

```go
func TestJoinPermissionResolvesItsNode(t *testing.T) {
	var none *v1alpha1.JoinPermission
	if got := none.ResolvedNode("vip"); got != "" {
		t.Errorf("no rule resolved to %q, want empty", got)
	}
	if none.DenyOnly() {
		t.Error("no rule reads as deny-only")
	}
	if got := (&v1alpha1.JoinPermission{}).ResolvedNode("vip"); got != "spawnery.join.vip" {
		t.Errorf("default node = %q, want spawnery.join.vip", got)
	}
	named := &v1alpha1.JoinPermission{Node: "network.vip", Mode: v1alpha1.JoinPermissionDenyOnly}
	if got := named.ResolvedNode("vip"); got != "network.vip" {
		t.Errorf("named node = %q, want network.vip", got)
	}
	if !named.DenyOnly() {
		t.Error("DenyOnly mode does not read as deny-only")
	}
	if (&v1alpha1.JoinPermission{Mode: v1alpha1.JoinPermissionRequired}).DenyOnly() {
		t.Error("Required mode reads as deny-only")
	}
}
```

Check the import alias the file already uses for the package (`v1alpha1` or `spawneryv1alpha1`) and match it.

- [ ] **Step 2: Run it and see it fail**

Run: `NIX develop /home/paul/git/spawnery-join -c go test ./api/v1alpha1/ -run TestJoinPermissionResolvesItsNode -count=1`
Expected: build failure, `undefined: v1alpha1.JoinPermission`.

- [ ] **Step 3: Add the types and the field** to `api/v1alpha1/servergroup_types.go`. Types beside the other enums:

```go
// +kubebuilder:validation:Enum=Required;DenyOnly
type JoinPermissionMode string

const (
	JoinPermissionRequired JoinPermissionMode = "Required"
	JoinPermissionDenyOnly JoinPermissionMode = "DenyOnly"
)

// JoinPermission is the permission that decides who may join a group's
// servers. It is read at runtime over the agent channel; changing it
// restarts no server.
type JoinPermission struct {
	// Node is the permission node. Empty means spawnery.join.<group name>.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-z0-9_.-]+$`
	// +optional
	Node string `json:"node,omitempty"`

	// Mode Required admits only players who hold the node. DenyOnly admits
	// everyone except players for whom the node is explicitly set to false.
	// +kubebuilder:default=Required
	// +optional
	Mode JoinPermissionMode `json:"mode,omitempty"`
}

func (j *JoinPermission) ResolvedNode(group string) string {
	if j == nil {
		return ""
	}
	if j.Node != "" {
		return j.Node
	}
	return "spawnery.join." + group
}

func (j *JoinPermission) DenyOnly() bool {
	return j != nil && j.Mode == JoinPermissionDenyOnly
}
```

Field in `ServerGroupSpec`, directly after `EnforcePlayableSlots`:

```go
	// JoinPermission limits who may join this group's servers. A proxy routes
	// a player around a group they may not join; the server refuses the login.
	// On an OnDemand group only the proxy checks it.
	// +optional
	JoinPermission *JoinPermission `json:"joinPermission,omitempty"`
```

- [ ] **Step 4: Regenerate and run the unit test**

Run: `NIX develop /home/paul/git/spawnery-join -c make manifests generate`
Then: `NIX develop /home/paul/git/spawnery-join -c go test ./api/v1alpha1/ -run TestJoinPermissionResolvesItsNode -count=1`
Expected: PASS. `git diff --stat` lists `config/crd/bases/spawnery.cloud_servergroups.yaml`, `charts/spawnery/templates/crds.yaml`, `api/v1alpha1/zz_generated.deepcopy.go`, `docs/reference/crds.md`.

- [ ] **Step 5: Write the envtest validation tests** in `api/v1alpha1/servergroup_envtest_test.go` (helpers `ephemeralGroup`, `onDemandGroup`, `testenv.Client`, `testenv.Namespace` already exist there):

```go
func TestJoinPermissionModeDefaultsToRequired(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	g := ephemeralGroup(ns, "vip")
	g.Spec.JoinPermission = &spawneryv1alpha1.JoinPermission{}
	if err := c.Create(ctx, g); err != nil {
		t.Fatalf("create: %v", err)
	}
	var got spawneryv1alpha1.ServerGroup
	if err := c.Get(ctx, client.ObjectKeyFromObject(g), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.JoinPermission == nil || got.Spec.JoinPermission.Mode != spawneryv1alpha1.JoinPermissionRequired {
		t.Errorf("joinPermission = %+v, want mode Required", got.Spec.JoinPermission)
	}
}

func TestJoinPermissionRefusesABadNodeOrMode(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	for i, jp := range []spawneryv1alpha1.JoinPermission{
		{Node: "Network.VIP"},
		{Node: "network vip"},
		{Node: strings.Repeat("a", 129)},
		{Mode: "Sometimes"},
	} {
		g := ephemeralGroup(ns, fmt.Sprintf("bad-%d", i))
		g.Spec.JoinPermission = &jp
		if err := c.Create(ctx, g); err == nil {
			t.Errorf("%+v was accepted", jp)
		}
	}
}

func TestJoinPermissionIsAllowedOnEveryType(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	p := persistentGroup(ns, "persistent-join")
	p.Spec.JoinPermission = &spawneryv1alpha1.JoinPermission{Node: "network.build"}
	if err := c.Create(ctx, p); err != nil {
		t.Fatalf("persistent: %v", err)
	}
	o := onDemandGroup(ns, "ondemand-join")
	o.Spec.JoinPermission = &spawneryv1alpha1.JoinPermission{Mode: spawneryv1alpha1.JoinPermissionDenyOnly}
	if err := c.Create(ctx, o); err != nil {
		t.Fatalf("on-demand: %v", err)
	}
}
```

- [ ] **Step 6: Run the envtests**

Run: `NIX develop /home/paul/git/spawnery-join -c go test ./api/v1alpha1/ -run 'TestJoinPermission' -count=1`
Expected: PASS (all three envtests plus the unit test).

- [ ] **Step 7: Commit**

```bash
git add api/v1alpha1 config/crd charts/spawnery/templates/crds.yaml docs/reference/crds.md
git commit -m "feat(api): spec.joinPermission on ServerGroup" -m "<body: what the field is, Required as default, the default node name>"
```

---

### Task 2: The wire

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto` (`message GroupState`, after `enforce_playable_slots = 10`)
- Modify: `internal/netstate/netstate.go:104-115` (the `GroupState` literal for server groups)
- Test: `internal/netstate/netstate_test.go`, `internal/agentpb/contract_test.go`
- Regenerate: `internal/agentpb/`, `agent/common/src/proto/java/`

**Interfaces:**
- Consumes: `JoinPermission.ResolvedNode`, `JoinPermission.DenyOnly` (Task 1).
- Produces: Go `agentpb.GroupState.JoinPermission string`, `JoinPermissionDenyOnly bool`; Java `GroupState.getJoinPermission(): String`, `getJoinPermissionDenyOnly(): boolean`.

- [ ] **Step 1: Write the failing tests.** In `internal/netstate/netstate_test.go` (helpers `source`, `ephemeralGroup`, `onDemandGroup` exist):

```go
func TestBuildCarriesAGroupsJoinRule(t *testing.T) {
	required := ephemeralGroup("ns", "build")
	required.Spec.JoinPermission = &spawneryv1alpha1.JoinPermission{}
	denyOnly := ephemeralGroup("ns", "hub")
	denyOnly.Spec.JoinPermission = &spawneryv1alpha1.JoinPermission{
		Node: "network.banned", Mode: spawneryv1alpha1.JoinPermissionDenyOnly,
	}
	src, _ := source(t, required, denyOnly, ephemeralGroup("ns", "lobby"))

	got, err := src.Build(context.Background(), "ns", netstate.ForServers)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Sorted: build, hub, lobby.
	if g := got.GetGroups()[0]; g.GetJoinPermission() != "spawnery.join.build" || g.GetJoinPermissionDenyOnly() {
		t.Errorf("build = %+v, want spawnery.join.build, required", g)
	}
	if g := got.GetGroups()[1]; g.GetJoinPermission() != "network.banned" || !g.GetJoinPermissionDenyOnly() {
		t.Errorf("hub = %+v, want network.banned, deny-only", g)
	}
	if g := got.GetGroups()[2]; g.GetJoinPermission() != "" {
		t.Errorf("lobby = %+v, want no rule", g)
	}
}

func TestAnOnDemandGroupsJoinRuleReachesTheProxies(t *testing.T) {
	private := onDemandGroup("ns", "realms")
	private.Spec.JoinPermission = &spawneryv1alpha1.JoinPermission{}
	src, _ := source(t, private)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.GetGroups()) != 1 || got.GetGroups()[0].GetJoinPermission() != "spawnery.join.realms" {
		t.Errorf("groups = %+v, want realms with spawnery.join.realms", got.GetGroups())
	}
}
```

In `internal/agentpb/contract_test.go` add a test beside `TestOnDemandFieldNumbersAreFixed`:

```go
func TestJoinPermissionFieldNumbersAreFixed(t *testing.T) {
	md := (&agentpb.GroupState{}).ProtoReflect().Descriptor()
	for name, want := range map[protoreflect.Name]protoreflect.FieldNumber{
		"join_permission":           11,
		"join_permission_deny_only": 12,
	} {
		fd := md.Fields().ByName(name)
		if fd == nil {
			t.Errorf("GroupState has no field %s", name)
			continue
		}
		if fd.Number() != want {
			t.Errorf("GroupState.%s is field %d, want %d: a renumbered field is a silent wire break", name, fd.Number(), want)
		}
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `NIX develop /home/paul/git/spawnery-join -c go test ./internal/netstate/ ./internal/agentpb/ -run 'JoinRule|JoinPermission' -count=1`
Expected: build failure, `GetJoinPermission undefined`.

- [ ] **Step 3: Add the proto fields** at the end of `message GroupState`:

```proto
  // The permission that decides who may join this group's servers, already
  // resolved from spec.joinPermission. Empty means the group has no rule.
  string join_permission = 11;
  // spec.joinPermission.mode is DenyOnly: only an explicit false refuses.
  bool join_permission_deny_only = 12;
```

- [ ] **Step 4: Fill them in `netstate.Build`**, in the server-group literal after `EnforcePlayableSlots`:

```go
			JoinPermission:         g.Spec.JoinPermission.ResolvedNode(g.Name),
			JoinPermissionDenyOnly: g.Spec.JoinPermission.DenyOnly(),
```

- [ ] **Step 5: Regenerate and run the tests**

Run: `NIX develop /home/paul/git/spawnery-join -c make proto`
Then: `NIX develop /home/paul/git/spawnery-join -c go test ./internal/netstate/ ./internal/agentpb/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add proto internal/agentpb agent/common/src/proto internal/netstate
git commit -m "feat(agentpb): carry a group's join rule to every agent"
```

---

### Task 3: The decision and the mirror (agent/common)

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/JoinRules.kt`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/NetworkMirror.kt` (Snapshot field, `apply`, accessor)
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/JoinRulesTest.kt`, `agent/common/src/test/kotlin/cloud/spawnery/agent/NetworkMirrorTest.kt`

**Interfaces:**
- Consumes: `GroupState.getJoinPermission()`, `getJoinPermissionDenyOnly()` (Task 2).
- Produces:
  - `enum class PermissionValue { TRUE, FALSE, UNDEFINED }`
  - `data class JoinRule(val node: String, val denyOnly: Boolean)`
  - `object JoinRules { const val DENIED_KEY = "spawnery.join.denied"; const val DENIED_FALLBACK = "You may not join %s."; fun mayJoin(rule: JoinRule?, value: PermissionValue): Boolean; fun contexts(server: String, group: String, network: String): Map<String, String> }`
  - `NetworkMirror.joinRule(group: String): JoinRule?`

- [ ] **Step 1: Write the failing tests.** `JoinRulesTest.kt`:

```kotlin
package cloud.spawnery.agent

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

class JoinRulesTest {
    private val required = JoinRule("network.vip", denyOnly = false)
    private val denyOnly = JoinRule("network.banned", denyOnly = true)

    @Test
    fun `without a rule everybody may join`() {
        for (value in PermissionValue.entries) {
            assertEquals(true, JoinRules.mayJoin(null, value), "$value")
        }
    }

    @Test
    fun `required admits only a granted node`() {
        assertEquals(true, JoinRules.mayJoin(required, PermissionValue.TRUE))
        assertEquals(false, JoinRules.mayJoin(required, PermissionValue.FALSE))
        assertEquals(false, JoinRules.mayJoin(required, PermissionValue.UNDEFINED))
    }

    @Test
    fun `deny only refuses only an explicit false`() {
        assertEquals(true, JoinRules.mayJoin(denyOnly, PermissionValue.TRUE))
        assertEquals(false, JoinRules.mayJoin(denyOnly, PermissionValue.FALSE))
        assertEquals(true, JoinRules.mayJoin(denyOnly, PermissionValue.UNDEFINED))
    }

    @Test
    fun `the contexts are the target server's, as a backend registers them`() {
        assertEquals(
            mapOf("server" to "vip-x1", "group" to "vip", "network" to "tutorial", "environment" to "paper"),
            JoinRules.contexts("vip-x1", "vip", "tutorial"),
        )
    }

    @Test
    fun `a blank context is left out rather than matched as empty`() {
        assertEquals(
            mapOf("server" to "vip-x1", "group" to "vip", "environment" to "paper"),
            JoinRules.contexts("vip-x1", "vip", ""),
        )
    }
}
```

In `NetworkMirrorTest.kt` (look at how existing tests build a `NetworkState` and copy that style):

```kotlin
    @Test
    fun `a group's join rule is mirrored, and a group without one has none`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder()
                .addGroups(GroupState.newBuilder().setName("vip").setJoinPermission("network.vip"))
                .addGroups(
                    GroupState.newBuilder().setName("hub")
                        .setJoinPermission("network.banned").setJoinPermissionDenyOnly(true),
                )
                .addGroups(GroupState.newBuilder().setName("lobby"))
                .build(),
        )
        assertEquals(JoinRule("network.vip", denyOnly = false), mirror.joinRule("vip"))
        assertEquals(JoinRule("network.banned", denyOnly = true), mirror.joinRule("hub"))
        assertNull(mirror.joinRule("lobby"))
        assertNull(mirror.joinRule("nowhere"))
    }
```

- [ ] **Step 2: Run and see it fail**

Run: `git add -A && NIX build /home/paul/git/spawnery-join#agents --no-link -L 2>&1 | tail -30`
Expected: compile errors, `Unresolved reference: JoinRule`.

- [ ] **Step 3: Implement.** `JoinRules.kt`:

```kotlin
package cloud.spawnery.agent

enum class PermissionValue { TRUE, FALSE, UNDEFINED }

data class JoinRule(val node: String, val denyOnly: Boolean)

object JoinRules {
    const val DENIED_KEY = "spawnery.join.denied"
    const val DENIED_FALLBACK = "You may not join %s."

    fun mayJoin(rule: JoinRule?, value: PermissionValue): Boolean = when {
        rule == null -> true
        rule.denyOnly -> value != PermissionValue.FALSE
        else -> value == PermissionValue.TRUE
    }

    /** The keys [LuckPermsContexts] registers on a backend, so a proxy asks what the backend would. */
    fun contexts(server: String, group: String, network: String): Map<String, String> =
        linkedMapOf("server" to server, "group" to group, "network" to network, "environment" to "paper")
            .filterValues { it.isNotBlank() }
}
```

In `NetworkMirror.kt`: add `val joinRules: Map<String, JoinRule> = emptyMap(),` to `Snapshot` (after `acceptingTransfers`); in `apply` add

```kotlin
            joinRules = state.groupsList
                .filter { it.joinPermission.isNotEmpty() }
                .associate { it.name to JoinRule(it.joinPermission, it.joinPermissionDenyOnly) },
```

and the accessor

```kotlin
    fun joinRule(group: String): JoinRule? = snapshot.joinRules[group]
```

- [ ] **Step 4: Run and see it pass**

Run: `git add -A && NIX build /home/paul/git/spawnery-join#agents --no-link -L 2>&1 | grep -E "JoinRules|NetworkMirror|FAILED|BUILD" | tail -20`
Expected: `BUILD SUCCESSFUL` for both Gradle runs, no `FAILED`.

- [ ] **Step 5: Commit**

```bash
git add agent/common
git commit -m "feat(agent): decide a join from a group's rule and a permission value"
```

---

### Task 4: The backend check (Paper)

**Files:**
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/LoginGate.kt`
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/LoginGateListener.kt`
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt:187-195` (`registerLoginGateOnce`)
- Test: `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/LoginGateTest.kt`

**Interfaces:**
- Consumes: `JoinRules`, `JoinRule`, `PermissionValue`, `NetworkMirror.joinRule` (Task 3).
- Produces: `LoginGate.permissionValue(has: Boolean, isSet: Boolean): PermissionValue`; `LoginGate.denied(groupDisplayName: String): Component`; `LoginGate.displayName(group: String, displayName: String?): String`; `LoginGateListener(group, mirror, state, pending, log: (String) -> Unit)`.

- [ ] **Step 1: Write the failing tests** in `LoginGateTest.kt`:

```kotlin
    @Test
    fun `a granted node reads as true, whether or not it is set`() {
        assertEquals(PermissionValue.TRUE, LoginGate.permissionValue(has = true, isSet = true))
        // An op on a node nobody set: Bukkit's default for ops is true.
        assertEquals(PermissionValue.TRUE, LoginGate.permissionValue(has = true, isSet = false))
    }

    @Test
    fun `an explicit false reads as false and an unset node as undefined`() {
        assertEquals(PermissionValue.FALSE, LoginGate.permissionValue(has = false, isSet = true))
        assertEquals(PermissionValue.UNDEFINED, LoginGate.permissionValue(has = false, isSet = false))
    }

    @Test
    fun `the refusal is translatable and names the group`() {
        val refusal = LoginGate.denied("VIP Lobby") as TranslatableComponent
        assertEquals("spawnery.join.denied", refusal.key())
        assertEquals("You may not join %s.", refusal.fallback())
        assertEquals(Component.text("VIP Lobby"), refusal.arguments().single().asComponent())
    }

    @Test
    fun `a group without a display name is named by its name`() {
        assertEquals("vip", LoginGate.displayName("vip", ""))
        assertEquals("vip", LoginGate.displayName("vip", null))
        assertEquals("VIP Lobby", LoginGate.displayName("vip", "VIP Lobby"))
    }
```

Imports: `cloud.spawnery.agent.PermissionValue`, `net.kyori.adventure.text.Component`, `net.kyori.adventure.text.TranslatableComponent`. `LoginGate.kt` itself needs `cloud.spawnery.agent.JoinRules` and `cloud.spawnery.agent.PermissionValue`.

- [ ] **Step 2: Run and see it fail**

Run: `git add -A && NIX build /home/paul/git/spawnery-join#agents --no-link -L 2>&1 | tail -30`
Expected: compile errors, `Unresolved reference: permissionValue`.

- [ ] **Step 3: Implement in `LoginGate.kt`:**

```kotlin
    fun permissionValue(has: Boolean, isSet: Boolean): PermissionValue = when {
        has -> PermissionValue.TRUE
        isSet -> PermissionValue.FALSE
        else -> PermissionValue.UNDEFINED
    }

    fun denied(groupDisplayName: String): Component =
        Component.translatable()
            .key(JoinRules.DENIED_KEY)
            .fallback(JoinRules.DENIED_FALLBACK)
            .arguments(Component.text(groupDisplayName))
            .build()

    fun displayName(group: String, displayName: String?): String =
        displayName?.takeIf { it.isNotBlank() } ?: group
```

Use `displayName` for the existing full-round refusal too (replace the inline `takeIf` in the listener).

- [ ] **Step 4: Check the rule first in `LoginGateListener.onLogin`.** Add the constructor parameter `private val log: (String) -> Unit = {}` after `pending`. The method becomes:

```kotlin
    @EventHandler(priority = EventPriority.HIGH)
    fun onLogin(event: PlayerLoginEvent) {
        if (event.result != PlayerLoginEvent.Result.ALLOWED) return
        val rule = mirror.joinRule(group)
        if (rule != null) {
            val value = LoginGate.permissionValue(
                has = event.player.hasPermission(rule.node),
                isSet = event.player.isPermissionSet(rule.node),
            )
            if (!JoinRules.mayJoin(rule, value)) {
                event.disallow(PlayerLoginEvent.Result.KICK_OTHER, LoginGate.denied(groupName()))
                log("spawnery: refused '${event.player.name}' on group $group: ${rule.node} is $value")
                return
            }
        }
        val admission = mirror.admission(group) ?: return
        if (!admission.enforce) return
        val permission = LoginGate.permission(group)
        val bypass = event.player.hasPermission(permission)
        val playable = LoginGate.effectivePlayable(state.playable, admission.playableSlots, Bukkit.getMaxPlayers())
        val seated = Bukkit.getOnlinePlayers().count { !it.hasPermission(permission) }
        if (!LoginGate.admits(true, bypass, seated, pending.count(), playable)) {
            event.disallow(PlayerLoginEvent.Result.KICK_FULL, LoginGate.refusal(groupName()))
            return
        }
        if (!bypass) pending.admit(event.player.uniqueId)
    }

    private fun groupName(): String =
        LoginGate.displayName(group, mirror.groups().firstOrNull { it.name() == group }?.displayName())
```

The `refused '…'` log line is what the e2e test in Task 8 reads; keep its wording.

- [ ] **Step 5: Register the listener for a join rule too.** In `AgentPlugin.registerLoginGateOnce`:

```kotlin
    private fun registerLoginGateOnce() {
        if (loginGate != null) return
        val group = System.getenv("SPAWNERY_GROUP") ?: return
        if (mirror.admission(group)?.enforce != true && mirror.joinRule(group) == null) return
        val gate = LoginGateListener(group, mirror, state, log = logger::info)
        server.pluginManager.registerEvents(gate, this)
        loginGate = gate
        logger.info("group $group enforces playable slots or a join permission; login check registered")
    }
```

- [ ] **Step 6: Run and see it pass**

Run: `git add -A && NIX build /home/paul/git/spawnery-join#agents --no-link -L 2>&1 | grep -E "LoginGate|FAILED|BUILD" | tail -20`
Expected: `BUILD SUCCESSFUL`, no `FAILED`.

- [ ] **Step 7: Commit**

```bash
git add agent/paper
git commit -m "feat(agent): refuse a login the group's join permission does not allow"
```

---

### Task 5: Permission lookup on the proxy

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/LuckPermsPermissions.kt`
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/JoinPermissions.kt`
- Test: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/JoinPermissionsTest.kt`

**Interfaces:**
- Consumes: `JoinRules`, `JoinRule`, `PermissionValue` (Task 3).
- Produces:
  - `fun interface PermissionLookup { fun value(player: UUID, node: String, contexts: Map<String, String>): PermissionValue? }` (null = this lookup cannot answer)
  - `fun interface JoinAccess { fun mayJoin(player: UUID, server: String, group: String): Boolean; companion object { val OPEN: JoinAccess } }`
  - `class JoinPermissions(rules: (String) -> JoinRule?, network: () -> String, lookups: List<PermissionLookup>) : JoinAccess`
  - `object LuckPermsPermissions { fun lookupIfPresent(): ((UUID, String, Map<String, String>) -> PermissionValue?)? }` in `agent/common` (a plain function type, because `PermissionLookup` lives in the Velocity module)
  - `class VelocityPermissionLookup(proxy: ProxyServer) : PermissionLookup`

- [ ] **Step 1: Write the failing tests** in `JoinPermissionsTest.kt`:

```kotlin
package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.JoinRule
import cloud.spawnery.agent.PermissionValue
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.util.UUID

class JoinPermissionsTest {
    private val player = UUID.fromString("00000000-0000-0000-0000-000000000001")
    private val rules = mapOf("vip" to JoinRule("network.vip", denyOnly = false))

    @Test
    fun `a group without a rule is open and nobody is asked`() {
        val access = JoinPermissions(rules::get, { "tutorial" }, listOf(PermissionLookup { _, _, _ -> error("asked") }))
        assertTrue(access.mayJoin(player, "lobby-1", "lobby"))
    }

    @Test
    fun `the lookup is asked in the target server's contexts`() {
        var asked: Map<String, String>? = null
        val access = JoinPermissions(rules::get, { "tutorial" }, listOf(
            PermissionLookup { _, node, contexts ->
                asked = contexts
                if (node == "network.vip") PermissionValue.TRUE else PermissionValue.UNDEFINED
            },
        ))
        assertTrue(access.mayJoin(player, "vip-x1", "vip"))
        assertEquals(
            mapOf("server" to "vip-x1", "group" to "vip", "network" to "tutorial", "environment" to "paper"),
            asked,
        )
    }

    @Test
    fun `a lookup that cannot answer hands over to the next`() {
        val access = JoinPermissions(rules::get, { "tutorial" }, listOf(
            PermissionLookup { _, _, _ -> null },
            PermissionLookup { _, _, _ -> PermissionValue.FALSE },
        ))
        assertFalse(access.mayJoin(player, "vip-x1", "vip"))
    }

    @Test
    fun `no lookup answering reads as undefined`() {
        val access = JoinPermissions(rules::get, { "tutorial" }, listOf(PermissionLookup { _, _, _ -> null }))
        assertFalse(access.mayJoin(player, "vip-x1", "vip"))
    }

    @Test
    fun `a lookup that throws counts as no answer`() {
        val access = JoinPermissions(rules::get, { "tutorial" }, listOf(
            PermissionLookup { _, _, _ -> throw IllegalStateException("LuckPerms is not loaded") },
            PermissionLookup { _, _, _ -> PermissionValue.TRUE },
        ))
        assertTrue(access.mayJoin(player, "vip-x1", "vip"))
    }
}
```

- [ ] **Step 2: Run and see it fail**

Run: `git add -A && NIX build /home/paul/git/spawnery-join#agents --no-link -L 2>&1 | tail -30`
Expected: compile errors, `Unresolved reference: JoinPermissions`.

- [ ] **Step 3: Implement.** `JoinPermissions.kt`:

```kotlin
package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.JoinRule
import cloud.spawnery.agent.JoinRules
import cloud.spawnery.agent.PermissionValue
import com.velocitypowered.api.permission.Tristate
import com.velocitypowered.api.proxy.ProxyServer
import java.util.UUID

fun interface PermissionLookup {
    fun value(player: UUID, node: String, contexts: Map<String, String>): PermissionValue?
}

fun interface JoinAccess {
    fun mayJoin(player: UUID, server: String, group: String): Boolean

    companion object {
        val OPEN = JoinAccess { _, _, _ -> true }
    }
}

class JoinPermissions(
    private val rules: (String) -> JoinRule?,
    private val network: () -> String,
    private val lookups: List<PermissionLookup>,
) : JoinAccess {
    override fun mayJoin(player: UUID, server: String, group: String): Boolean {
        val rule = rules(group) ?: return true
        val contexts = JoinRules.contexts(server, group, network())
        val value = lookups.firstNotNullOfOrNull { lookup ->
            runCatching { lookup.value(player, rule.node, contexts) }.getOrNull()
        } ?: PermissionValue.UNDEFINED
        return JoinRules.mayJoin(rule, value)
    }
}

/** Velocity's own answer knows no contexts; it decides only where LuckPerms cannot. */
class VelocityPermissionLookup(private val proxy: ProxyServer) : PermissionLookup {
    override fun value(player: UUID, node: String, contexts: Map<String, String>): PermissionValue? =
        proxy.getPlayer(player).map {
            when (it.getPermissionValue(node)) {
                Tristate.TRUE -> PermissionValue.TRUE
                Tristate.FALSE -> PermissionValue.FALSE
                else -> PermissionValue.UNDEFINED
            }
        }.orElse(null)
}
```

`LuckPermsPermissions.kt` in `agent/common` (LuckPerms API is `compileOnly` there already):

```kotlin
package cloud.spawnery.agent

import net.luckperms.api.LuckPermsProvider
import net.luckperms.api.context.ImmutableContextSet
import net.luckperms.api.query.QueryOptions
import net.luckperms.api.util.Tristate
import java.util.UUID

object LuckPermsPermissions {
    /** Probed for the same reason as [LuckPermsContexts.registerIfPresent]. */
    fun lookupIfPresent(): ((UUID, String, Map<String, String>) -> PermissionValue?)? {
        try {
            Class.forName("net.luckperms.api.LuckPermsProvider")
        } catch (_: ClassNotFoundException) {
            return null
        }
        return ::value
    }

    private fun value(player: UUID, node: String, contexts: Map<String, String>): PermissionValue? {
        val user = LuckPermsProvider.get().userManager.getUser(player) ?: return null
        val set = ImmutableContextSet.builder().apply { contexts.forEach { (k, v) -> add(k, v) } }.build()
        return when (user.cachedData.getPermissionData(QueryOptions.contextual(set)).checkPermission(node)) {
            Tristate.TRUE -> PermissionValue.TRUE
            Tristate.FALSE -> PermissionValue.FALSE
            else -> PermissionValue.UNDEFINED
        }
    }
}
```

`agent/common` has no Velocity types, so it returns a plain function; `AgentPlugin` (Task 6) wraps it: `LuckPermsPermissions.lookupIfPresent()?.let { f -> PermissionLookup(f::invoke) }`. A `LuckPermsProvider.get()` that throws because LuckPerms is not loaded is caught by `JoinPermissions`' `runCatching`.

- [ ] **Step 4: Run and see it pass**

Run: `git add -A && NIX build /home/paul/git/spawnery-join#agents --no-link -L 2>&1 | grep -E "JoinPermissions|FAILED|BUILD" | tail -20`
Expected: `BUILD SUCCESSFUL`, no `FAILED`.

- [ ] **Step 5: Commit**

```bash
git add agent/common agent/velocity
git commit -m "feat(agent): look up a join permission in the target server's contexts"
```

---

### Task 6: Routing around a forbidden group (Velocity)

**Files:**
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/Router.kt`
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/ServerDirectory.kt` (add `groupOf`)
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/Drain.kt` (constructor, `move`)
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/Rescue.kt` (constructor, `target`)
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt` (`start`, `onChooseInitialServer`, new `ServerPreConnectEvent` handler)
- Test: `RouterTest.kt`, `ServerDirectoryTest.kt`, `DrainTest.kt`, `RescueTest.kt` in `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/`

**Interfaces:**
- Consumes: `JoinAccess`, `JoinPermissions`, `PermissionLookup`, `VelocityPermissionLookup` (Task 5); `NetworkMirror.joinRule` (Task 3); `JoinRules.DENIED_KEY`, `DENIED_FALLBACK`.
- Produces: `Router.choose(groups, excluding = emptySet(), mayJoin: (server: String, group: String) -> Boolean = { _, _ -> true })`; `ServerDirectory.groupOf(server: String): String?`; `Drain(players, router, log, access: JoinAccess = JoinAccess.OPEN)`; `Rescue(router, log, access: JoinAccess = JoinAccess.OPEN)`.

- [ ] **Step 1: Write the failing tests.** `RouterTest.kt`:

```kotlin
    @Test
    fun `a group the player may not join is skipped like an empty one`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("vip-1", "10.0.0.1:25565", "vip"),
                Backend("lobby-1", "10.0.0.2:25565", "lobby"),
            ),
        )
        val router = Router(directory)

        val chosen = router.choose(listOf("vip", "lobby")) { _, group -> group != "vip" }

        assertEquals("lobby-1", chosen?.serverInfo?.name)
    }

    @Test
    fun `the predicate sees each candidate server with its group`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("vip-1", "10.0.0.1:25565", "vip"),
                Backend("vip-2", "10.0.0.2:25565", "vip"),
            ),
        )
        val router = Router(directory)

        val chosen = router.choose(listOf("vip")) { server, group -> group == "vip" && server == "vip-2" }

        assertEquals("vip-2", chosen?.serverInfo?.name)
    }

    @Test
    fun `every group forbidden yields null`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(listOf(Backend("vip-1", "10.0.0.1:25565", "vip")))
        val router = Router(directory)

        assertNull(router.choose(listOf("vip")) { _, _ -> false })
    }
```

`ServerDirectoryTest.kt`:

```kotlin
    @Test
    fun `groupOf names a registered server's group and nothing for a stranger`() {
        val directory = ServerDirectory(FakeRegistry()) { _, _ -> }
        directory.apply(listOf(Backend("vip-1", "10.0.0.1:25565", "vip")))
        assertEquals("vip", directory.groupOf("vip-1"))
        assertEquals("vip", directory.groupOf("VIP-1"))
        assertNull(directory.groupOf("from-an-overlay"))
    }
```

`DrainTest.kt` and `RescueTest.kt`: read the existing first test of each file and add one test per file in the same fixture style that builds `Backend("vip-1", …, "vip")` and `Backend("lobby-1", …, "lobby")`, drains or rescues a player with `toGroups = listOf("vip", "lobby")`, passes `access = JoinAccess { _, _, group -> group != "vip" }`, and asserts the target is `lobby-1`. Names: `` `a drained player is not sent into a group they may not join` `` and `` `a rescued player is not sent into a group they may not join` ``.

- [ ] **Step 2: Run and see it fail**

Run: `git add -A && NIX build /home/paul/git/spawnery-join#agents --no-link -L 2>&1 | tail -30`
Expected: compile errors on the new `choose` argument, `groupOf` and `access`.

- [ ] **Step 3: Implement the router, directory, drain and rescue.**

`Router.choose`:

```kotlin
    fun choose(
        groups: List<String>,
        excluding: Collection<String> = emptySet(),
        mayJoin: (server: String, group: String) -> Boolean = { _, _ -> true },
    ): RegisteredServer? {
        for (group in groups) {
            val candidates = directory.inGroup(group)
                .filter { candidate -> excluding.none { candidate.serverInfo.name.equals(it, ignoreCase = true) } }
                .filter { mayJoin(it.serverInfo.name, group) }
            if (candidates.isEmpty()) continue

            return candidates.minWithOrNull(compareBy({ it.playersConnected.size }, { it.serverInfo.name }))
        }
        return null
    }
```

Keep the existing comments in that method that still apply (the one on emptiness after exclusion now also covers the predicate; reword it to "after the exclusion and the join rule").

`ServerDirectory`:

```kotlin
    @Synchronized
    fun groupOf(server: String): String? = backends[server.lowercase()]?.group
```

`Drain`: add `private val access: JoinAccess = JoinAccess.OPEN,` as the last constructor parameter; in `move`:

```kotlin
        val target = router.choose(toGroups, excluding = excluded) { server, group ->
            access.mayJoin(player.uuid, server, group)
        } ?: return false
```

`Rescue`: add `private val access: JoinAccess = JoinAccess.OPEN,` as the last constructor parameter; in `target`:

```kotlin
        val target = router.choose(toGroups, excluding = chain) { server, group ->
            access.mayJoin(player, server, group)
        }
```

- [ ] **Step 4: Wire it in `AgentPlugin.start`.** Add fields `private var directory: ServerDirectory? = null` and `private var joinAccess: JoinAccess = JoinAccess.OPEN`. After `val self = …` and before `Drain`/`Rescue` are built:

```kotlin
        this.directory = directory
        val access = JoinPermissions(
            rules = mirror::joinRule,
            network = self::network,
            lookups = listOfNotNull(
                LuckPermsPermissions.lookupIfPresent()?.let { f -> PermissionLookup { p, n, c -> f(p, n, c) } },
                VelocityPermissionLookup(proxy),
            ),
        )
        this.joinAccess = access
```

Pass `access` to `Rescue(router, ::warn, access)` and `Drain(players, router, ::warn, access)`. `Rescue` is built before `self` today (line ~161); move its construction below `access`.

- [ ] **Step 5: The first server and the transfer landing.** Replace `onChooseInitialServer`:

```kotlin
    @Subscribe
    fun onChooseInitialServer(event: PlayerChooseInitialServerEvent) {
        val player = event.player.uniqueId
        val mayJoin = { server: String, group: String -> joinAccess.mayJoin(player, server, group) }
        val arrival = transfers?.landing(player, event.player.username)
            ?.let { proxy.getServer(it).orElse(null) }
            ?.takeIf { server ->
                val group = directory?.groupOf(server.serverInfo.name)
                group == null || mayJoin(server.serverInfo.name, group)
            }
        val target = arrival ?: router?.choose(fallbackGroups, mayJoin = mayJoin) ?: run {
            if (router != null) {
                logger.warn(
                    "spawnery: no server '${event.player.username}' may join in $fallbackGroups " +
                        "(empty, or closed to them by a join permission); letting the proxy refuse the connection",
                )
            }
            return
        }
        event.setInitialServer(target)
    }
```

`Rescue` and `Drain` carry `access` themselves, so `onKickedFromServer` and the drain callbacks need no change.

- [ ] **Step 6: Refuse a targeted connect.** New handler next to the existing `onServerPreConnect` (keep that one, it runs last at `Short.MIN_VALUE` and already skips a denied result):

```kotlin
    @Subscribe
    fun onServerPreConnectJoinRule(event: ServerPreConnectEvent) {
        if (!event.result.isAllowed) return
        val target = event.result.server.orElse(event.originalServer)
        val name = target.serverInfo.name
        val group = directory?.groupOf(name) ?: return
        if (joinAccess.mayJoin(event.player.uniqueId, name, group)) return
        event.result = ServerPreConnectEvent.ServerResult.denied()
        val shown = mirror.groups().firstOrNull { it.name() == group }?.displayName()?.takeIf { it.isNotBlank() } ?: group
        event.player.sendMessage(
            Component.translatable()
                .key(JoinRules.DENIED_KEY)
                .fallback(JoinRules.DENIED_FALLBACK)
                .arguments(Component.text(shown))
                .build(),
        )
    }
```

Imports: `cloud.spawnery.agent.JoinRules`, `cloud.spawnery.agent.LuckPermsPermissions`, `net.kyori.adventure.text.Component`.

- [ ] **Step 7: Run and see it pass**

Run: `git add -A && NIX build /home/paul/git/spawnery-join#agents --no-link -L 2>&1 | grep -E "Router|Drain|Rescue|Directory|FAILED|BUILD" | tail -30`
Expected: `BUILD SUCCESSFUL`, no `FAILED`.

- [ ] **Step 8: Commit**

```bash
git add agent/velocity
git commit -m "feat(agent): route a player around a group they may not join"
```

---

### Task 7: Docs and the plugin API's word on connect

**Files:**
- Create: `docs/guides/join-permission.md`
- Modify: `mkdocs.yml` (nav, under Guides after `Group environment: guides/group-environment.md`)
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java` (Javadoc of `connect`, ~line 74-89)
- Modify: `docs/guides/on-demand-servers.md` (one sentence: only the proxy checks a join permission on an on-demand group)

**Interfaces:** none.

- [ ] **Step 1: Write the guide** `docs/guides/join-permission.md`. Content, in this order, plain prose, no em dashes:
  1. Title `# Who may join a group`.
  2. The YAML from spec §2 and one paragraph per mode: `Required` admits only holders (an administrator with `*` passes); `DenyOnly` admits everyone except players with the node set to `false`, with the LuckPerms command `/lp user <name> permission set <node> false`.
  3. Scoping a grant with the four contexts (`server`, `group`, `network`, `environment`), example `/lp group vip permission set network.vip true group=vip-lobby`.
  4. What a player sees: the proxy skips a fallback group they may not join and tries the next; `/server` and other connects are refused with `You may not join <group>.`; the backend refuses a login the proxy let through. Key `spawnery.join.denied` for networks with their own translations.
  5. Limits: the check runs on join only; on an `OnDemand` group only the proxy checks; a group with a join rule turns Paper's reconfiguration API off on its servers (same as `enforcePlayableSlots`); a plugin's `connect()` answers `ordered` even when the player is then refused.
  6. Changing the rule restarts nothing; it applies within one network sync.

- [ ] **Step 2: Add the nav entry** in `mkdocs.yml`: `      - Join permissions: guides/join-permission.md` after the `Group environment` line.

- [ ] **Step 3: Extend the `connect` Javadoc** with one paragraph:

```java
     * <p>The operator does not know permissions. A player sent to a group
     * whose join permission refuses them is still answered {@code ordered},
     * and is then refused by the proxy or the server.
```

- [ ] **Step 4: Add the on-demand sentence** to `docs/guides/on-demand-servers.md` where the page lists what applies to members (search for `playableSlots` or `enforcePlayableSlots`; if neither appears, add it at the end of the section describing the group spec): `A spec.joinPermission on an on-demand group is checked by the proxies only: a member's server does not see its own group.`

- [ ] **Step 5: Build the docs site and run the length check**

Run: `git add -A && NIX build /home/paul/git/spawnery-join#docs-site --no-link 2>&1 | tail -5; NIX develop /home/paul/git/spawnery-join -c bash hack/docs-length.sh`
Expected: build succeeds (mkdocs `--strict`), `docs-length.sh` exits 0.

- [ ] **Step 6: Commit**

```bash
git add docs mkdocs.yml agent/api
git commit -m "feat(docs): guide for spec.joinPermission"
```

---

### Task 8: End to end on real images

**Files:**
- Modify: `test/e2e/tutorial_test.go` (new test after `TestTutorialTransferOnDrain`)

**Interfaces:**
- Consumes: the `refused '<user>' on group <group>` log line (Task 4); helpers in this file: `applyManifest`, `eventuallyIn`, `startHeldJoin`, `whereIs`, `readPodLog`, `podReady`, `hasEnv`, constants `tutorialNamespace`, `tutorialProxyGroup`, `tutorialServerGroup`, `tutorialOperatorNamespace`; `podspec.EnvFallbackGroups`, `podspec.LabelRole`, `podspec.RoleProxy`, `podspec.ServerContainerName` (check the exact constant name for the Paper container in `internal/podspec`).

- [ ] **Step 1: Write the test.** Shape (fill in exactly; reuse helpers, do not copy them):

```go
// TestTutorialJoinPermission puts a vip group ahead of the lobby in the
// gateway's fallback list. The test images carry no LuckPerms, so nobody
// holds a node: with Required the player lands in the lobby without the vip
// server ever refusing them, and with DenyOnly they land in vip.
func TestTutorialJoinPermission(t *testing.T) {
	if os.Getenv("SPAWNERY_E2E_TUTORIAL") != "1" {
		t.Skip("set SPAWNERY_E2E_TUTORIAL=1; hack/e2e-tutorial.sh does this nightly")
	}
	joinPath, err := exec.LookPath("spawnery-join")
	if err != nil {
		t.Fatalf("spawnery-join not on PATH (%v); the dev shell carries it, run this through nix develop", err)
	}
	applyManifest(t, tutorialManifest)
	// ... see the numbered list below
}
```

Body, in order:
1. Read the lobby `ServerGroup`; create `vip` as a copy of its spec with `Scaling{MinReplicas: 1, MaxReplicas: 1, SpareSlots: 0}`, CPU request `100m` (the nightly runner has 4 vCPUs, see the comment in `TestTutorialTransferOnDrain`), and `JoinPermission: &JoinPermission{Mode: JoinPermissionRequired}`. `t.Cleanup` deletes it.
2. Patch the gateway `ProxyGroup`'s `spec.routing.fallbackGroups` to `["vip", "lobby"]`; `t.Cleanup` restores the old list.
3. `eventuallyIn(…, 5*time.Minute, …)` until one Ready `vip` server exists and every Ready gateway pod has `SPAWNERY_FALLBACK_GROUPS` set to `vip,lobby` (use `hasEnv` and read the value; check `podspec` for how the list is joined).
4. Wait 15 s for at least two network syncs, so every proxy and the vip server mirror the rule. (A sleep is acceptable here: there is no observable for "the agent has applied this sync". Write it as `time.Sleep` with that reason in one comment line.)
5. `j := startHeldJoin(t, joinPath, "required", 20*time.Second)`; `_, server := whereIs(t, j, gatewayPods)`; `j.stop()`. Assert `server` belongs to the lobby group (prefix `lobby-` is not reliable; look the name up in the `ServerList` and compare `Spec.GroupRef.Name`).
6. Read the vip server pod's log (`readPodLog`) and assert it contains no `refused 'required'`: the proxy routed around the group instead of sending the player into a refusal.
7. Patch vip's `JoinPermission.Mode` to `DenyOnly`; sleep 15 s as in step 4; join as `"denyonly"`; assert the server belongs to `vip`.

`gatewayPods` is a `func() ([]corev1.Pod, error)` listing pods with `podspec.LabelRole: podspec.RoleProxy` in `tutorialNamespace`; `TestTutorialTransferOnDrain` builds one, reuse its shape.

- [ ] **Step 2: Run it**

Run (rootless podman on `paul-desktop`): `systemd-run --scope --user --property=Delegate=yes -- NIX develop /home/paul/git/spawnery-join -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e-tutorial`
Expected: `TestTutorialJoinPermission` PASS along with the other tutorial tests. The images must carry this branch's agents: `hack/e2e-tutorial.sh` builds and loads them; check its header if a tag must be passed.

- [ ] **Step 3: Prove it bites.** In a throwaway worktree (`git worktree add --detach /tmp/claude-…/mut HEAD`), change `Router.choose` so the `.filter { mayJoin(…) }` line is removed, run the same `make e2e-tutorial` there with `-run TestTutorialJoinPermission` if the script allows it, and record the failure (step 6 must fail: the vip server refused `required`). Remove the worktree. Paste both outputs into the task report.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/tutorial_test.go
git commit -m "test(e2e): a join permission routes a player around a group on real images"
```

---

### Task 9: Full checks and release 0.18.0

**Files:**
- Modify: `flake.nix` (`imageVersion`, `operatorVersion`), `charts/spawnery/Chart.yaml` (`version`, `appVersion`), `charts/spawnery/values.yaml` (`image.tag`), the install lines in `README.md` and `charts/spawnery/README.md`, every image tag pin that `hack/image-tag-pins-agree.sh` reports (tutorial manifest, docs), `docs/reference/chart-values.md` (regenerated).

- [ ] **Step 1: Run every gate**

Run: `NIX develop /home/paul/git/spawnery-join -c make test` then `NIX develop /home/paul/git/spawnery-join -c make lint`, then `git add -A && NIX build /home/paul/git/spawnery-join#agents --no-link`.
Expected: all green; `go test` shows no FAIL; `golangci-lint` `0 issues.`

- [ ] **Step 2: Bump the versions.** Use `f87d279` (`chore: 0.17.0`) as the pattern for which files move. All three numbers move to `0.18.0`: `imageVersion` (agents changed), `operatorVersion` and `appVersion` and `values.yaml` `image.tag` (netstate changed), chart `version` (CRD changed). Then `NIX develop /home/paul/git/spawnery-join -c bash hack/image-tag-pins-agree.sh` and update every pin it names to `0.18.0`, then `make manifests`.

- [ ] **Step 3: Check the release preflight**

Run: `git diff v0.17.0 HEAD --stat -- charts/ agent/ image/ internal/render`
Expected: `charts/` and `agent/` have changes, and their numbers moved in step 2.

- [ ] **Step 4: Run every gate again** as in step 1.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "chore: 0.18.0, spec.joinPermission" -m "<body: the feature in two sentences; which numbers move and why>"
```

The PR is opened by the lead after the final review. Tagging happens only after Paul has merged it, and after `gh run list --workflow=ci.yml --limit 3` on master is green.
