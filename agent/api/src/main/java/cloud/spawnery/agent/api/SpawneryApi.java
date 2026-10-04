/*
Copyright paul_wtf.

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
 * <p><b>The methods that return a {@link CompletionStage} ask the operator, and
 * can fail; every other method is answered locally.</b> The operator keeps a
 * mirror current in each agent, so a read -- {@link #self()} through
 * {@link #player} -- never crosses a network, blocks, or fails: there is no
 * timeout and no exception to handle. What it returns is the last thing the
 * operator said, which during a reconnect may be a few seconds old and is
 * never wrong about a moment that happened. Each stage-returning method says
 * how it can fail; {@link #holdReadiness} is local and throws on a proxy.
 *
 * <p><b>Consume this interface; do not implement it.</b> New methods break
 * implementors but no caller. The agent supplies the implementation; a plugin
 * obtains it from {@link Spawnery#api()}.
 *
 * <p>Everything is scoped to this pod's own namespace. No method reaches
 * another network: the agent's credentials are a pod-bound ServiceAccount
 * token, so there is nothing to widen.
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

    /** Every proxy in this network, in no particular order. */
    List<ProxyInfo> proxies();

    /** One proxy by name, empty if this network has none. */
    Optional<ProxyInfo> proxy(String name);

    /** Every player on this network, whichever backend they are on. */
    List<CloudPlayer> players();

    /** One player by UUID, empty if they are not on this network. */
    Optional<CloudPlayer> player(UUID id);

    /**
     * Whether this player's connection to this proxy is a transfer from
     * another proxy of the network, carrying a cookie this agent verified.
     *
     * <p>For a plugin that announces joins: a player who arrived this way
     * was already online a moment ago, and to their friends nothing happened.
     * A transfer intent alone is not enough for that, because any client can
     * claim one; this is true only when the signed cookie checked out.
     *
     * <p>Known from {@code LoginEvent} on, for a listener that runs after the
     * agent's own (the agent asks for the cookie there and holds the login
     * until it has the answer). Stays true until the player disconnects from
     * this proxy. Always false on a server, and on a proxy whose group does
     * not have {@code spec.update.transfer}.
     *
     * <p>Agents older than 0.19.0 do not have this method.
     */
    boolean arrivedByTransfer(UUID player);

    /**
     * Whether this proxy has sent this player to another proxy of the
     * network, because this proxy is leaving.
     *
     * <p>For a plugin that cleans up after a player: the disconnect that
     * follows is not a quit. True from the moment the transfer is sent until
     * every listener of the player's {@code DisconnectEvent} on this proxy has
     * run. It says the player was sent, not that they arrived; a client that
     * fails to reconnect is gone like any other, so a plugin that keeps state
     * for them should still let it lapse if they do not turn up elsewhere.
     * Always false on a server.
     *
     * <p>Agents older than 0.19.0 do not have this method.
     */
    boolean leavingByTransfer(UUID player);

    /**
     * Asks the operator to move a player.
     *
     * <p><b>Asynchronous on both platforms</b>, although a proxy could answer
     * locally, so that the same code compiles on either side.
     *
     * <p>The operator does not know permissions. A player sent to a group
     * whose join permission refuses them is still answered {@code ordered},
     * and is then refused by the proxy or the server.
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
     * <p>The stage completes with no value. It fails when the operator
     * refuses, and <b>asking for a server that is already retiring is a
     * failure</b>, so a caller can tell "you retired it" from "somebody had
     * already asked".
     */
    CompletionStage<Void> retire(String server);

    /**
     * Takes a server's retirement back. It takes joins again, and nothing
     * automatic removes it any more; it stays until it ends by itself.
     *
     * <p>Fails for a server that is already stopping or is not retiring, with
     * the operator's reason.
     */
    CompletionStage<Void> unretire(String server);

    /**
     * How this network is doing: CPU and memory of its pods against what they
     * asked for, per group, and each server's tick rate. Asks the operator;
     * never anything outside this network's namespace.
     */
    CompletionStage<NetworkStatus> status();

    /**
     * {@link #status()} for one server group, proxy group, server or proxy,
     * looked up in that order. Fails when nothing on this network has the name.
     */
    CompletionStage<NetworkStatus> status(String target);

    /**
     * Asks for extra capacity on a group, for a while.
     *
     * <p><b>It adds to what the group tries for and never to what it may
     * reach.</b> The group's own {@code maxReplicas} still binds. A request
     * for more than the ceiling leaves is refused rather than trimmed.
     *
     * <p><b>It expires.</b> Pass {@code null} for the operator's default,
     * which is an hour. The operator also bounds how long you may ask for; a
     * lasting need belongs in the group's own definition.
     *
     * <p>Boosts add rather than replace: two calls make two boosts.
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
     * <p>Every one, not the newest. Zero is an ordinary answer -- the group
     * had no boosts -- and not a failure.
     */
    CompletionStage<Integer> stopBoosts(String group);

    /**
     * Asks for the private server that carries this key.
     *
     * <p>The key names a world and not a server: the operator composes the
     * server's name from the group and the key, and the same key later finds
     * the same world. Only a group of type {@code OnDemand} has members to
     * ask for; anything else fails.
     *
     * <p><b>The stage completing means the server was asked for, not that it
     * can take players.</b> It starts out as any server does; watch
     * {@link ServerInfo#phase()} or the events for {@link ServerPhase#READY}
     * before sending anybody there.
     *
     * <p>Asking for one that is already running succeeds with
     * {@link StartedServer#alreadyRunning()} set, so a player pressing a
     * button twice needs no lock on your side. That flag says the server
     * exists and nothing about its being ready.
     *
     * <p><b>How it fails.</b> When the operator answers no, the stage fails
     * with an {@link IllegalStateException} whose message is
     * {@code <REASON>: <message>} -- the operator's reason, then its own
     * sentence. {@code handle}, {@code exceptionally} and {@code whenComplete}
     * called directly on this stage receive that exception itself. A stage
     * derived from it with {@code thenApply} or {@code thenCompose} delivers
     * it wrapped in a {@code CompletionException}, and {@code get()} wraps it
     * in an {@code ExecutionException}; unwrap with {@code getCause()} only
     * when what you caught is one of those wrappers. The reasons:
     * <ul>
     *   <li>{@code REFUSED} for a key that cannot be part of a name, for a name
     *       longer than 63 characters once the group and the key are composed,
     *       for a group that is not on-demand, for a name that another group's
     *       server already has, and for a group that is genuinely at its
     *       {@code spec.maxInstances}.</li>
     *   <li>{@code NOT_FOUND} for a group this network does not have.</li>
     *   <li>{@code UNAVAILABLE} for a member of that key that is still
     *       stopping, and for a group that is at its {@code maxInstances}
     *       only because one of its members is stopping. Neither is a refusal:
     *       the same request succeeds once that member is gone, so ask
     *       again.</li>
     * </ul>
     *
     * <p>Two failures are not the operator's answer and carry no reason: the
     * stage fails with a {@link java.util.concurrent.TimeoutException} when no
     * answer arrives within ten seconds, and with an
     * {@link IllegalStateException} when the stream was renewed while the
     * request was in flight, which is failed rather than retried because only
     * you know whether asking twice is safe. For a start it is: one that was in
     * fact carried out is answered {@code alreadyRunning} the second time.
     */
    CompletionStage<StartedServer> startServer(String group, String key);

    /**
     * Stops one private server and leaves its world where it is.
     *
     * <p>Not {@link #retire}: retiring closes a server's door and lets it
     * empty in its own time, while this says its owner is done with it — the
     * players on it are moved through the proxies and the pod goes. The world
     * is on a claim nothing deletes, so the next {@link #startServer} of the
     * same key finds it.
     *
     * <p>The stage completes with no value. Stopping a server that is already
     * on its way out succeeds too, so a second press of the same button is not
     * an error.
     *
     * <p>It fails with {@code REFUSED} for a server that is not a member of an
     * on-demand group, which is what keeps a wrong name from taking down a
     * lobby, and with {@code NOT_FOUND} for a name this network does not have.
     * The failure has the shape {@link #startServer} describes, the timeout and
     * the renewed stream included. Asking again after either is safe, but a
     * stop that was in fact carried out may answer {@code NOT_FOUND} the second
     * time, once the server is gone.
     */
    CompletionStage<Void> stopServer(String server);

    /**
     * Deletes one private server for good: stops it if it is running, as
     * {@link #stopServer} does, and deletes its world. There is no undo.
     *
     * <p>Group and key as {@link #startServer} takes them, because a stopped
     * server's world is all that is left of it. The stage completes when the
     * deletion is under way; a {@link #startServer} of the same key answers
     * {@code UNAVAILABLE} until the world is gone, and then starts an empty
     * one.
     *
     * <p>It fails with {@code NOT_FOUND} for a group this network does not have
     * or a key with neither a server nor a world, and with {@code REFUSED} for
     * a group that is not on-demand, a key no name can be built from, or a
     * world this operator did not make. The failure has the shape
     * {@link #startServer} describes, the timeout and the renewed stream
     * included; asking again is safe, and answers {@code NOT_FOUND} once the
     * world is gone.
     */
    CompletionStage<Void> deleteServer(String group, String key);

    /**
     * Opens or closes this server's own door.
     *
     * <p><b>Closing is not {@link #retire}.</b> Retiring says the server is
     * finished: it stops taking joins, empties out, and is taken down once it
     * is empty. Closing only stops the joins, for as long as you like, and can
     * be taken back.
     *
     * <p><b>Nobody is moved.</b> The players already here go on playing until
     * they leave on their own. What changes is only whether the proxies send
     * anybody new.
     *
     * <p><b>The phase does not change, and neither does the routing table.</b>
     * A closed server is still {@link ServerPhase#READY} — the phase is the
     * operator's account of a server's lifecycle, and shutting a door is not a
     * lifecycle event. The server also stays in the proxies' table, so a
     * spectator sent there by name still arrives. What changes is who is sent
     * there unasked: the closed server's empty seats stop counting as
     * capacity, so the next player looking for a game is placed somewhere
     * else. To leave the table as well, say the round is over —
     * {@link #endRound()}.
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
     * Says this server's round is over.
     *
     * <p>Two things follow, and only the operator can do either: the server
     * leaves the proxies' routing table, so nobody new arrives at a round that
     * has finished, and the pod stopping after this reads as an ending rather
     * than a fault — its group replaces it without counting a failure.
     *
     * <p>It does not stop the server. Call it when the round is over and let
     * the process end as it always did.
     *
     * <p>The stage fails when the operator refuses — most plainly on a proxy,
     * which is not in anybody's routing table but is the routing table.
     *
     * <p>Like {@link #acceptJoins}, it survives a reconnection without being
     * called again: the agent restates it on every new session, because the
     * operator's default for a session it has never seen is that the round
     * has not ended. It is restated together with the door, never apart —
     * a round that ended closed the door too, and the two are never told
     * apart on the wire.
     */
    CompletionStage<Void> endRound();

    /**
     * Sets how many of this server's seats count as capacity, from now until
     * changed: what its group's spare slots, free slots and a connect to the
     * group measure. Players beyond it are still admitted up to the server's
     * limit, and make the server full rather than overfull -- unless the group
     * sets enforcePlayableSlots, in which case it is also the door.
     *
     * <p>It asks the operator nothing. The next periodic report carries it,
     * and every report after that, so a new session restates it without a
     * second call.
     *
     * <p>Servers only; a proxy throws {@link UnsupportedOperationException}.
     *
     * @param slots the playable seats; 0 hands the decision back to the
     *     group's {@code spec.playableSlots}. A figure above the server's
     *     slots counts as its slots.
     * @throws IllegalArgumentException if {@code slots} is negative
     */
    void playableSlots(int slots);

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
     * <p><b>The cloud carries this and never reads it.</b> No operator
     * decision looks at it. It reaches the other agents in this network as
     * {@link ServerInfo#state()} and {@link ServerInfo#attributes()} and goes
     * no further.
     *
     * <p><b>It is not {@link ServerInfo#phase()}.</b> The phase is the
     * operator's account of a server's lifecycle and no plugin can write it.
     * This is the server's own account of what happens while it is
     * {@code READY}.
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
     * session.
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
