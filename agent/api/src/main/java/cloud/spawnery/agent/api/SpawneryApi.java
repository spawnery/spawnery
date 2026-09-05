/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloud.spawnery.agent.api;

import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.UUID;
import java.util.concurrent.CompletionStage;

/**
 * What a plugin can ask the cloud, from either side of the proxy.
 *
 * <p><b>Every method here is a local read.</b> The operator keeps a mirror
 * current in each agent, so none of these calls crosses a network, blocks, or
 * fails -- there is no timeout and no exception to handle. What they return is
 * the last thing the operator said, which during a reconnect may be a few
 * seconds old and is never wrong about a moment that happened.
 *
 * <p><b>Consume this interface; do not implement it.</b> Methods are added
 * here as later milestones land -- events, moving a player, starting a server
 * -- and adding one breaks an implementor while leaving every caller alone.
 * The agent supplies the implementation; a plugin obtains it from
 * {@link Spawnery#api()}.
 *
 * <p>Everything is scoped to this pod's own namespace, which is the whole of
 * what this agent can see. There is no method that reaches another network,
 * and that is structural rather than a check somebody could forget: the
 * agent's credentials are a pod-bound ServiceAccount token, so there is
 * nothing to widen.
 */
public interface SpawneryApi {
    /** What this process is. Use {@code instanceof} to learn which side. */
    Self self();

    /** Every group in this network, in no particular order. */
    List<Group> groups();

    /** One group by name, empty if this network has none. */
    Optional<Group> group(String name);

    /** Every server in this network, in no particular order. */
    List<ServerInfo> servers();

    /** One server by name, empty if this network has none. */
    Optional<ServerInfo> server(String name);

    /** Every player on this network, whichever backend they are on. */
    List<CloudPlayer> players();

    /** One player by UUID, empty if they are not on this network. */
    Optional<CloudPlayer> player(UUID id);

    /**
     * Asks the operator to move a player.
     *
     * <p><b>Asynchronous on both platforms, including the one where it need
     * not be.</b> On a proxy this could answer locally; on a backend it is a
     * round trip through the operator. Following the platform would make the
     * signature synchronous on one side and not the other, and a plugin author
     * moving between them would have to rewrite rather than recompile. So it
     * is the shape of the harder case on both.
     *
     * <p>The stage fails rather than returning a result when the operator
     * refuses or cannot answer — including when the stream was renewed while
     * the request was in flight, which is failed rather than retried because
     * only you know whether moving that player twice is safe.
     *
     * <p>A player who logged out between your call and the operator reading it
     * is an ordinary failure and not a bug. So is a target that is not
     * routable yet.
     */
    CompletionStage<ConnectResult> connect(UUID player, Target to);

    /**
     * Asks that one server stop taking joins and empty out.
     *
     * <p>Retiring is not stopping. The server takes no further joins, the
     * players on it finish in their own time, and nobody is moved or
     * disconnected. Emptied, it is taken down by the same rules that take down
     * any server its group no longer needs.
     *
     * <p>Asynchronous on both platforms for the reason {@link #connect} is,
     * and on both it is a round trip: this one changes an object in the
     * cluster, so neither a proxy nor a backend could answer it locally even
     * in principle.
     *
     * <p>The stage completes with no value — the operator's answer names the
     * server you already named. It fails when the operator refuses, and
     * <b>asking for a server that is already retiring is a failure</b>: the
     * operator distinguishes "you retired it" from "somebody had already
     * asked", and a caller that wants to treat the second as success can do so
     * far more safely than one that was never told.
     */
    CompletionStage<Void> retire(String server);

    /**
     * Asks for extra capacity on a group, for a while.
     *
     * <p><b>It adds to what the group tries for and never to what it may
     * reach.</b> The group's own {@code maxReplicas} still binds — a ceiling
     * is an instruction, and a call from inside a game server must not be able
     * to lift one. A request for more than the ceiling leaves is refused
     * rather than trimmed, so what you asked for is what you got or you were
     * told why not.
     *
     * <p><b>It expires.</b> Pass {@code null} for the operator's default,
     * which is an hour. The operator also bounds how long you may ask for; a
     * need that outlives an evening belongs in the group's own definition,
     * where a person reviews it, and this call deliberately cannot make one.
     *
     * <p>Boosts add rather than replace. Two calls make two boosts, and the
     * second does not overwrite the first — which is what makes "somebody
     * else already boosted this" a non-event rather than a race.
     *
     * <p>The stage fails when the operator refuses: a group it does not have,
     * a group sized by a fixed replica count rather than by scaling, more than
     * the ceiling leaves, or longer than it allows. Each says which.
     *
     * @param forHowLong how long the boost should run, or {@code null} for the
     *     operator's default.
     */
    CompletionStage<BoostResult> boost(String group, int replicas, Duration forHowLong);

    /**
     * Ends every boost on a group and reports how many there were.
     *
     * <p>Every one, not the newest: a partial reduction across boosts with
     * different expiries is arithmetic nobody asked for. Zero is an ordinary
     * answer — the group had no boosts — and not a failure, which is what a
     * caller who expected some needs to be able to tell.
     */
    CompletionStage<Integer> stopBoosts(String group);

    /**
     * Opens or closes this server's own door.
     *
     * <p><b>Closing is not {@link #retire}, and the difference is the whole
     * reason this exists.</b> Retiring says the server is finished: it stops
     * taking joins, empties out, and is taken down once it is empty. This says
     * only the first of those, it says it for as long as you like, and you can
     * take it back. A round that has started is not a server that is going
     * away, and asking for the one when you mean the other reads as a
     * decommissioning to everybody who looks at it afterwards.
     *
     * <p><b>Nobody is moved.</b> The players already here go on playing until
     * they leave on their own. What changes is only whether the proxies send
     * anybody new.
     *
     * <p><b>The phase does not change.</b> A closed server is still
     * {@link ServerPhase#READY} — the phase is the operator's account of a
     * server's lifecycle, and shutting a door is not a lifecycle event. What
     * changes is {@link ServerInfo#registered()}, which is the field a caller
     * choosing where to send somebody already reads.
     *
     * <p>Your group notices. A closed server's empty seats stop counting as
     * the group's free capacity, so a group sized by spare slots builds a
     * replacement rather than sitting at its floor while every server in it
     * has shut its door.
     *
     * <p>The stage fails when the operator refuses — most plainly on a proxy,
     * which is not in anybody's routing table but is the routing table.
     *
     * <p>Like {@link #announce}, it survives a reconnection without being
     * called again: the agent restates the last door state on every new
     * session, because the operator's default for a session it has never seen
     * is open.
     *
     * @param accept {@code false} closes the door, {@code true} opens it.
     */
    CompletionStage<Void> acceptJoins(boolean accept);

    /**
     * Holds this server back from readiness until the returned hold is closed.
     *
     * <p>For a plugin whose initialisation continues after the server has
     * finished enabling -- a mapping table loaded on its own executor, a
     * database opened in the background. The agent reports readiness when the
     * last hold is released <em>and</em> the server has finished enabling,
     * whichever comes second, so a server that is not finished stays
     * {@code Starting} rather than becoming {@code Ready} with nobody able to
     * play on it.
     *
     * <p><b>It cannot lower a readiness already reported.</b> Readiness is a
     * one-way latch; a hold taken after the agent has reported does nothing
     * but log. To stop new players reaching a server that is already ready,
     * use {@link #acceptJoins}, which is what that method is for.
     *
     * <p>A hold that is never released pins the server in {@code Starting}
     * until the operator's startup deadline fails it. That is the intended
     * outcome -- a plugin that never finishes starting is a broken server --
     * and {@code reason} is what names it in the log.
     *
     * <p>Servers only. A proxy has no readiness of this kind and this throws
     * {@link UnsupportedOperationException} there.
     *
     * @param reason what is being waited for, for the log. Required.
     */
    ReadinessHold holdReadiness(String reason);

    /**
     * Publishes what this server is doing, for every other server to read.
     *
     * <p><b>The cloud carries this and never reads it.</b> Nothing the
     * operator decides looks at a word of it: not where a player is sent, not
     * when a server is replaced, not how a group is sized. It reaches the
     * other agents in this network as {@link ServerInfo#state()} and
     * {@link ServerInfo#attributes()} and goes no further, which is what makes
     * it safe to put anything in and useless to put an instruction in.
     *
     * <p><b>It is not {@link ServerInfo#phase()}.</b> The phase is the
     * operator's account of a server's lifecycle and no plugin can write it.
     * This is the server's own account of itself, and the two are meant to
     * disagree: a server is {@code READY} from the moment it can take players
     * until it stops, and what is happening inside that window is a question
     * only the thing running there can answer.
     *
     * <p><b>Each call replaces the last one whole.</b> Attributes are not
     * merged: a call with one attribute leaves this server with one, whatever
     * the call before it said. Publish the whole description each time, which
     * is also the only way an attribute can ever be taken back.
     *
     * <p>The stage fails when the operator refuses -- a state or an attribute
     * longer than it carries, more attributes than it carries, or a call from
     * a proxy, which has no per-instance record in the network's picture for
     * an announcement to appear in. Each refusal says which.
     *
     * <p>It survives a reconnection without being called again: the agent
     * holds the last description it published and re-publishes it on every new
     * session, so an operator that restarts does not leave a running game
     * described as nothing.
     *
     * @param state what this server is doing, in a word or a short phrase.
     *     Empty clears it.
     * @param attributes anything else worth publishing. Empty clears them.
     */
    CompletionStage<Void> announce(String state, Map<String, String> attributes);

    /**
     * Where to hear about things happening in the cloud.
     *
     * <p>The same object every time, so a plugin may hold it. See
     * {@link EventBus} for what it does and does not promise — most of all
     * that it is a feed and not a ledger.
     */
    EventBus events();
}
