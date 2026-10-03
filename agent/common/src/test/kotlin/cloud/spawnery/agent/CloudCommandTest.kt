package cloud.spawnery.agent

import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.SpawneryApi
import cloud.spawnery.agent.pb.CloudRequest
import cloud.spawnery.agent.pb.BoostResult
import cloud.spawnery.agent.pb.CloudResponse
import cloud.spawnery.agent.pb.GroupState
import cloud.spawnery.agent.pb.RequestError
import cloud.spawnery.agent.pb.ProxyState
import cloud.spawnery.agent.pb.RetireResult
import cloud.spawnery.agent.pb.UnretireResult
import cloud.spawnery.agent.pb.StopBoostResult
import cloud.spawnery.agent.pb.NetworkState
import cloud.spawnery.agent.pb.ServerState
import cloud.spawnery.agent.pb.StatusResult
import com.mojang.brigadier.CommandDispatcher
import com.mojang.brigadier.exceptions.CommandSyntaxException
import java.util.UUID
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

private fun aNetwork(): NetworkState =
    NetworkState.newBuilder()
        .addGroups(
            GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL)
                .setReplicas(2).setReadyReplicas(1).setOnlinePlayers(12).setFreeSlots(88),
        )
        .addServers(
            ServerState.newBuilder().setName("lobby-a").setGroup("lobby")
                .setPhase("Ready").setPlayers(12).setSlots(100).setRegistered(true),
        )
        .build()

private fun aNetworkWithProxies(): NetworkState =
    NetworkState.newBuilder()
        .addGroups(
            GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL)
                .setReplicas(2).setReadyReplicas(2).setOnlinePlayers(3).setFreeSlots(97),
        )
        .addGroups(
            GroupState.newBuilder().setName("gateway").setKind(GroupState.Kind.PROXY)
                .setReplicas(2).setReadyReplicas(2).setOnlinePlayers(3),
        )
        .addServers(
            ServerState.newBuilder().setName("lobby-r").setGroup("lobby")
                .setPhase("Retiring").setPlayers(3).setSlots(100),
        )
        .addServers(
            ServerState.newBuilder().setName("lobby-h").setGroup("lobby")
                .setPhase("Ready").setPlayers(0).setSlots(100).setRegistered(true).setHeld(true),
        )
        .addProxies(ProxyState.newBuilder().setName("gateway-a").setGroup("gateway").setReady(true).setPlayers(3))
        .addProxies(ProxyState.newBuilder().setName("gateway-b").setGroup("gateway").setReady(true).setDraining(true))
        .build()

private fun aNetworkWithNode(node: String): NetworkState =
    NetworkState.newBuilder()
        .addGroups(
            GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL)
                .setReplicas(1).setReadyReplicas(1).setOnlinePlayers(12).setFreeSlots(88),
        )
        .addServers(
            ServerState.newBuilder().setName("lobby-a").setGroup("lobby")
                .setPhase("Ready").setPlayers(12).setSlots(100).setRegistered(true).setNode(node),
        )
        .build()

class CloudCommandTest {
    private val sent = mutableListOf<String>()
    private var permissions =
        setOf(PERMISSION_READ, PERMISSION_RETIRE, PERMISSION_SCALE, PERMISSION_EVENTS, PERMISSION_STATUS)
    private val asked = mutableListOf<String>()

    private val adapter = object : SourceAdapter<Int> {
        override fun hasPermission(source: Int, permission: String): Boolean {
            asked += permission
            return permission in permissions
        }

        override fun send(source: Int, message: String) {
            sent += message
        }

        override fun playerId(source: Int): UUID? = sourcePlayer
    }

    /** Null is the console. */
    private var sourcePlayer: UUID? = UUID.nameUUIDFromBytes("admin".toByteArray())

    private val feed = FeedState()

    /** Bare, so assertions about wording are not about the wrapper. */
    private var format = Feed.MESSAGE_TOKEN

    private val requested = mutableListOf<CloudRequest>()
    private val requests = Requests(timeoutMillis = 1_000, clock = System::currentTimeMillis)
    private val connector = CloudConnector(requests) { request -> requested += request }

    private fun api(state: NetworkState = aNetwork()): SpawneryApi = MirrorApi(
        NetworkMirror().also { it.apply(state) },
        object : ProxySelf {
            override fun name(): String = "gateway-0"
            override fun group(): String = "gateway"
            override fun network(): String = "production"
        },
        connector,
        CloudEvents(),
    )

    private fun run(command: String, api: SpawneryApi = api()): Int {
        val dispatcher = CommandDispatcher<Int>()
        dispatcher.register(cloudCommand(api, adapter, feed, { format }))
        return dispatcher.execute(command, 0)
    }

    private fun answer(build: CloudResponse.Builder.() -> Unit) {
        val id = requested.single().id
        connector.answer(CloudResponse.newBuilder().setId(id).apply(build).build())
    }

    @Test
    fun `command output wears the network's own format`() {
        format = "<gray>PREFIX</gray> ${Feed.MESSAGE_TOKEN}"

        run("cloud list")

        val line = sent.first()
        assertTrue(line.startsWith("<gray>PREFIX</gray> "), line)
        assertTrue(sent.any { plain(it).contains("lobby") }, "the answer itself was lost: $sent")
    }

    @Test
    fun `every reply wears it, not only the first`() {
        format = "<gray>PREFIX</gray> ${Feed.MESSAGE_TOKEN}"

        run("cloud start lobby 2")
        answer {
            setBoost(
                BoostResult.newBuilder().setReplicas(2)
                    .setExpiresAtUnix(java.time.Instant.parse("2026-08-30T20:00:00Z").epochSecond),
            )
        }

        assertEquals(3, sent.size, sent.toString())
        for (line in sent) {
            assertTrue(line.startsWith("<gray>PREFIX</gray> "), "unwrapped: $line")
        }
    }

    @Test
    fun `a blank format still says something`() {
        // Blank is what an operator older than the field sends.
        format = ""

        run("cloud list")

        val line = plain(sent.first())
        assertTrue(line.contains("Spawnery"), "the default format was not used: $line")
        assertTrue(sent.any { plain(it).contains("lobby") }, "the answer itself was lost: $sent")
    }

    @Test
    fun `list names every group and what it is doing`() {
        run("cloud list")

        assertTrue(sent.any { plain(it).contains("lobby") }, "the output named no group: $sent")
        assertTrue(
            sent.any { plain(it).contains("12/100 players") },
            "the output did not say what the group is doing: $sent",
        )
    }

    @Test
    fun `a source without the permission cannot see the command at all`() {
        // Brigadier's requires hides the branch rather than refusing it.
        permissions = emptySet()

        assertFailsWith<CommandSyntaxException> { run("cloud list") }
        assertTrue(sent.isEmpty(), "an unpermitted source was sent something: $sent")
    }

    @Test
    fun `info about a server names its phase and whether it takes joins`() {
        run("cloud info lobby-a")

        val line = sent.first()
        assertTrue(line.contains("lobby-a") && line.contains("READY"), line)
        // Registration and phase disagree during a drain.
        assertTrue(line.contains("taking joins"), line)
    }

    @Test
    fun `a server that says what it is doing has it in the line, marked as its own word`() {
        val described = NetworkState.newBuilder(aNetwork()).also { builder ->
            builder.setServers(
                0,
                ServerState.newBuilder(builder.getServers(0)).setState("running"),
            )
        }.build()

        run("cloud info lobby-a", api = api(described))

        val line = plain(sent.single { plain(it).contains("Says") })
        assertTrue(line.contains("running"), line)
    }

    @Test
    fun `a server that has said nothing gets no fragment rather than an empty one`() {
        run("cloud info lobby-a")

        assertTrue(sent.none { plain(it).contains("Says") }, "$sent")
    }

    @Test
    fun `taking joins and not taking joins are told apart by colour, not only by words`() {
        run("cloud info lobby-a")

        val line = sent.first()
        assertTrue(line.contains("<green>taking joins</green>"), line)
        assertTrue(plain(line).contains("taking joins"), plain(line))
    }

    @Test
    fun `an operator message carrying a tag reaches chat as text`() {
        // An unescaped `<` would be eaten by the parser, or make it throw
        // inside a network callback, which costs the session.
        run("cloud retire lobby-a")

        answer {
            setError(
                RequestError.newBuilder()
                    .setReason(RequestError.Reason.REFUSED)
                    .setMessage("room for <red>0</red>, not 9"),
            )
        }

        val line = sent.single()
        assertTrue(line.contains("\\<red>"), "the operator's tag was not escaped: $line")
        assertTrue(plain(line).contains("room for <red>0</red>, not 9"),
            "the operator's words did not survive: ${plain(line)}")
    }

    @Test
    fun `info about a group works through the same argument`() {
        run("cloud info lobby")

        assertTrue(sent.any { plain(it).contains("88 free") }, sent.toString())
    }

    @Test
    fun `info about something absent names what was asked for`() {
        run("cloud info nothing-here")

        assertTrue(sent.single().contains("nothing-here"), "the answer did not name it: $sent")
    }

    @Test
    fun `the tree asks the platform for nothing but the permissions it declares`() {
        // What this really guards is SourceAdapter staying two methods.
        run("cloud list")
        run("cloud info lobby-a")
        run("cloud retire lobby-a")
        run("cloud start lobby")
        run("cloud stop lobby")
        run("cloud events off")

        assertEquals(
            setOf(PERMISSION_READ, PERMISSION_RETIRE, PERMISSION_SCALE, PERMISSION_EVENTS),
            asked.toSet(),
        )
    }

    @Test
    fun `retire asks the operator and says what retiring means`() {
        run("cloud retire lobby-a")

        assertEquals("lobby-a", requested.single().retire.server)
        assertTrue(sent.isEmpty(), "the command answered before the operator did: $sent")

        answer { setRetire(RetireResult.newBuilder().setServer("lobby-a")) }

        val line = sent.single()
        assertTrue(line.contains("lobby-a") && line.contains("retiring"), line)
        assertTrue(line.contains("nobody is kicked"), "the output did not say what retiring does: $line")
    }

    @Test
    fun `a refusal reaches the source in the operator's own words`() {
        run("cloud retire lobby-a")

        answer {
            setError(
                RequestError.newBuilder()
                    .setReason(RequestError.Reason.REFUSED)
                    .setMessage("that server is already retiring"),
            )
        }

        val line = plain(sent.single())
        assertTrue(line.contains("already retiring"), "the operator's reason was lost: $line")
        assertFalse(line.contains("CompletionException"), "the future's wrapper reached chat: $line")
        assertTrue(line.startsWith("✘ could not retire"), "a refusal was worded as a success: $line")
    }

    @Test
    fun `an agent with no session says so instead of throwing at the platform`() {
        // The dormant seam throws; Brigadier would turn a throw out of executes
        // into "an internal error occurred".
        val dormant = MirrorApi(
            NetworkMirror().also { it.apply(aNetwork()) },
            object : ProxySelf {
                override fun name(): String = "gateway-0"
                override fun group(): String = "gateway"
                override fun network(): String = "production"
            },
            dormantConnector(),
            CloudEvents(),
        )

        run("cloud retire lobby-a", dormant)

        assertTrue(sent.single().contains("no session"), "the source was not told why: $sent")
    }

    @Test
    fun `start says what it created, that it is temporary, and where a lasting change lives`() {
        run("cloud start lobby 2 for 30m")

        val request = requested.single().boost
        assertEquals("lobby", request.group)
        assertEquals(2, request.replicas)
        assertEquals(1_800L, request.durationSeconds)

        answer {
            setBoost(
                BoostResult.newBuilder()
                    .setReplicas(2)
                    .setExpiresAtUnix(java.time.Instant.parse("2026-08-28T20:00:00Z").epochSecond),
            )
        }

        assertEquals(3, sent.size, "the three lines section 5.3 requires: $sent")
        assertTrue(sent[0].contains("+2 servers") && sent[0].contains("20:00"), sent[0])
        assertTrue(sent[1].contains("not a spec change"), sent[1])
        assertTrue(sent[1].contains("/cloud stop lobby"), "it did not say how to end it early: ${sent[1]}")
        assertTrue(sent[2].contains("edit the ServerGroup"), sent[2])
    }

    @Test
    fun `start without a count asks for one`() {
        run("cloud start lobby")

        assertEquals(1, requested.single().boost.replicas)
        assertEquals(0L, requested.single().boost.durationSeconds)
    }

    @Test
    fun `an unreadable duration is named rather than silently defaulted`() {
        run("cloud start lobby 2 for 2hh")

        assertTrue(requested.isEmpty(), "an unreadable duration still reached the operator: $requested")
        assertTrue(sent.single().contains("2hh"), "the answer did not name what it could not read: $sent")
    }

    @Test
    fun `stop says how many it removed`() {
        run("cloud stop lobby")

        assertEquals("lobby", requested.single().stopBoost.group)

        answer { setStopBoost(StopBoostResult.newBuilder().setRemoved(2)) }

        assertTrue(plain(sent.single()).contains("removed 2 boosts"), sent.toString())
    }

    @Test
    fun `stopping a group with no boosts says so plainly`() {
        run("cloud stop lobby")

        answer { setStopBoost(StopBoostResult.newBuilder().setRemoved(0)) }

        assertTrue(sent.single().contains("no boosts running"), sent.toString())
    }

    @Test
    fun `scaling is invisible without its own permission`() {
        permissions = setOf(PERMISSION_READ, PERMISSION_RETIRE)

        assertFailsWith<CommandSyntaxException> { run("cloud start lobby") }
        assertFailsWith<CommandSyntaxException> { run("cloud stop lobby") }
        assertTrue(requested.isEmpty(), "an unpermitted source reached the operator: $requested")
    }

    @Test
    fun `holding only scale still opens the root`() {
        permissions = setOf(PERMISSION_SCALE)

        run("cloud start lobby")

        assertEquals("lobby", requested.single().boost.group)
    }

    @Test
    fun `events off tells the player it lasts for this session only`() {
        run("cloud events off")

        val line = sent.single()
        assertTrue(line.contains("off"), line)
        assertTrue(
            line.contains("rejoin") || line.contains("session"),
            "it did not say the setting is for this session: $line",
        )
    }

    @Test
    fun `events off then on leaves the player wanting them again`() {
        run("cloud events off")
        assertFalse(feed.wants(sourcePlayer!!), "off did not take effect")

        run("cloud events on")
        assertTrue(feed.wants(sourcePlayer!!), "on did not undo off")
    }

    @Test
    fun `one player's opt-out is not another's`() {
        // A single boolean would have passed every other test here.
        val other = UUID.nameUUIDFromBytes("someone-else".toByteArray())
        run("cloud events off")

        assertFalse(feed.wants(sourcePlayer!!))
        assertTrue(feed.wants(other), "one player's opt-out silenced another")
    }

    @Test
    fun `the console is told it cannot opt out rather than silently failing`() {
        sourcePlayer = null

        run("cloud events off")

        assertTrue(sent.single().contains("console"), sent.toString())
    }

    @Test
    fun `events is invisible without its own permission`() {
        permissions = setOf(PERMISSION_READ)

        assertFailsWith<CommandSyntaxException> { run("cloud events off") }
    }

    @Test
    fun `holding only events still opens the root`() {
        permissions = setOf(PERMISSION_EVENTS)

        run("cloud events off")

        assertFalse(feed.wants(sourcePlayer!!))
    }

    @Test
    fun `a wrapped failure is unwrapped before it reaches chat`() {
        // MirrorApi delivers the cause directly, but an implementation that
        // derives its stage (thenApply, handle) delivers a CompletionException.
        val wrapping = object : SpawneryApi by api() {
            override fun retire(server: String): java.util.concurrent.CompletionStage<Void> {
                val failed = java.util.concurrent.CompletableFuture<Void>()
                failed.completeExceptionally(IllegalStateException("REFUSED: that server is already retiring"))
                return failed.thenApply { it }
            }
        }

        run("cloud retire lobby-a", wrapping)

        val line = sent.single()
        assertTrue(line.contains("already retiring"), "the operator's reason was lost: $line")
        assertFalse(line.contains("CompletionException"), "the future's wrapper reached chat: $line")
        assertFalse(line.contains("IllegalStateException"), "the exception class reached chat: $line")
    }

    @Test
    fun `retire is invisible without its own permission even to a reader`() {
        permissions = setOf(PERMISSION_READ)

        assertFailsWith<CommandSyntaxException> { run("cloud retire lobby-a") }
        assertTrue(requested.isEmpty(), "an unpermitted source reached the operator: $requested")
    }

    @Test
    fun `holding only retire still opens the root`() {
        permissions = setOf(PERMISSION_RETIRE)

        run("cloud retire lobby-a")

        assertEquals("lobby-a", requested.single().retire.server)
    }

    @Test
    fun `holding only retire still cannot read`() {
        permissions = setOf(PERMISSION_RETIRE)

        assertFailsWith<CommandSyntaxException> { run("cloud list") }
        assertTrue(sent.isEmpty(), "a source without the read permission was told something: $sent")
    }

    @Test
    fun `unretire asks the operator and says what it means`() {
        run("cloud unretire lobby-r", api(aNetworkWithProxies()))
        assertEquals("lobby-r", requested.single().unretire.server)
        assertTrue(sent.isEmpty(), "the command answered before the operator did: $sent")
        answer { setUnretire(UnretireResult.newBuilder().setServer("lobby-r")) }
        val line = sent.single()
        assertTrue(line.contains("lobby-r") && line.contains("takes joins again"), line)
    }

    @Test
    fun `unretire says why the operator refused`() {
        run("cloud unretire lobby-r", api(aNetworkWithProxies()))
        answer {
            setError(
                RequestError.newBuilder().setReason(RequestError.Reason.REFUSED)
                    .setMessage("that server is already stopping"),
            )
        }
        assertTrue(sent.single().contains("already stopping"), sent.single())
    }

    @Test
    fun `list shows a proxy group's proxies`() {
        run("cloud list", api(aNetworkWithProxies()))
        assertTrue(sent.any { it.contains("gateway-a") }, "$sent")
        assertTrue(sent.any { it.contains("gateway-b") && it.contains("draining") }, "$sent")
    }

    @Test
    fun `info answers for a proxy`() {
        run("cloud info gateway-b", api(aNetworkWithProxies()))
        assertTrue(sent.first().contains("gateway-b") && sent.first().contains("draining"), "$sent")
    }

    @Test
    fun `info says a held server is held`() {
        run("cloud info lobby-h", api(aNetworkWithProxies()))
        assertTrue(sent.any { it.contains("held") }, "$sent")
    }


    private fun statusAnswer(build: StatusResult.Builder.() -> Unit) =
        answer { setStatus(StatusResult.newBuilder().apply(build)) }

    private fun usage(cpuUsed: Long, cpuReq: Long, memUsed: Long, memReq: Long, pods: Int, measured: Int) =
        cloud.spawnery.agent.pb.ResourceUsage.newBuilder()
            .setCpuUsedMillicores(cpuUsed).setCpuRequestedMillicores(cpuReq).setCpuLimitMillicores(2 * cpuReq)
            .setMemoryUsedBytes(memUsed).setMemoryRequestedBytes(memReq).setMemoryLimitBytes(2 * memReq)
            .setPods(pods).setPodsMeasured(measured)

    @Test
    fun `status says why the operator refused`() {
        run("cloud status nowhere", api(aNetworkWithProxies()))
        answer {
            setError(
                RequestError.newBuilder().setReason(RequestError.Reason.NOT_FOUND)
                    .setMessage("no group, server or proxy by that name is on this network"),
            )
        }
        assertTrue(sent.single().contains("no group, server or proxy by that name"), sent.single())
    }

    @Test
    fun `status needs its own permission`() {
        permissions = setOf(PERMISSION_READ)
        assertFailsWith<CommandSyntaxException> { run("cloud status", api(aNetworkWithProxies())) }
    }

    @Test
    fun `status alone opens the cloud command`() {
        permissions = setOf(PERMISSION_STATUS)
        run("cloud status", api(aNetworkWithProxies()))
        assertEquals("", requested.single().status.target)
    }
    @Test
    fun `list opens with a heading and sorts groups into sections`() {
        run("cloud list", api(aNetworkWithProxies()))
        assertTrue(sent[0].contains("<bold>Network</bold>") && sent[0].contains("2 groups"), sent[0])
        val serverGroups = sent.indexOfFirst { it.contains("Server groups") }
        val proxyGroups = sent.indexOfFirst { it.contains("Proxy groups") }
        assertTrue(serverGroups in 1 until proxyGroups, "$sent")
        assertTrue(sent[serverGroups + 1].startsWith("   ") && sent[serverGroups + 1].contains("lobby"), "$sent")
        assertTrue(sent.any { it.startsWith("     ") && it.contains("gateway-b") && it.contains("draining") }, "$sent")
    }

    @Test
    fun `list of an empty network still says so`() {
        run("cloud list", api(NetworkState.getDefaultInstance()))
        assertTrue(sent.any { it.contains("no groups on this network yet") }, "$sent")
    }

    @Test
    fun `info of a server shows its node and a players bar`() {
        run("cloud info lobby-a", api(aNetworkWithNode("node-2")))
        assertTrue(sent[0].contains("<bold>lobby-a</bold>") && sent[0].contains("lobby"), sent[0])
        assertTrue(sent.any { it.contains("Node") && it.contains("node-2") }, "$sent")
        assertTrue(sent.any { it.contains("Players") && it.contains("|") && it.contains("12") }, "$sent")
    }

    @Test
    fun `info of an unscheduled server says so`() {
        run("cloud info lobby-a", api(aNetworkWithNode("")))
        assertTrue(sent.any { it.contains("Node") && it.contains("not scheduled") }, "$sent")
    }
    @Test
    fun `status shows resources with bars and groups in sections`() {
        run("cloud status", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = true
            players = 12; servers = 9; proxies = 2
            total = usage(3100, 8000, 12L shl 30, 24L shl 30, 11, 11).build()
            addGroups(
                cloud.spawnery.agent.pb.GroupStatus.newBuilder().setName("arena").setKind(GroupState.Kind.EPHEMERAL)
                    .setPhase("Ready").setReplicas(2).setReadyReplicas(2).setPlayers(6).setLowestTps(17.8)
                    .setUsage(usage(1100, 2000, 3L shl 30, 4L shl 30, 2, 2)),
            )
            addGroups(
                cloud.spawnery.agent.pb.GroupStatus.newBuilder().setName("gateway").setKind(GroupState.Kind.PROXY)
                    .setPhase("Ready").setReplicas(2).setReadyReplicas(2).setPlayers(12)
                    .setUsage(usage(200, 400, 1L shl 29, 1L shl 30, 2, 2)),
            )
            other = usage(500, 400, 1L shl 30, 1L shl 30, 2, 2).build()
        }
        assertTrue(sent[0].contains("<bold>Network status</bold>") && plain(sent[0]).contains("12 players"), sent[0])
        assertTrue(sent[1].contains("Resources"), sent[1])
        val cpu = sent.single { it.contains("CPU") && it.contains("cores") }
        assertTrue(cpu.contains("|") && plain(cpu).contains("3.1 of 16.0") && plain(cpu).contains("8.0 requested"), cpu)
        val ram = sent.single { it.contains("RAM") && it.contains("GiB") && it.contains("requested") }
        assertTrue(plain(ram).contains("12.0 of 48.0"), ram)
        val serverSection = sent.indexOfFirst { it.contains("Server groups") }
        val proxySection = sent.indexOfFirst { it.contains("Proxy groups") }
        assertTrue(sent[serverSection + 1].contains("arena") && sent[serverSection + 1].contains("<yellow>17.8"), "$sent")
        assertTrue(sent[proxySection + 1].contains("gateway") && !sent[proxySection + 1].contains("TPS"), "$sent")
        assertTrue(sent.any { it.contains("Other pods") }, "$sent")
    }

    @Test
    fun `status says when usage is unavailable`() {
        run("cloud status", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = false
            total = usage(0, 8000, 0, 24L shl 30, 11, 0).build()
        }
        assertTrue(sent.any { it.contains("metrics API not answering") }, "$sent")
        val cpu = sent.single { it.contains("CPU") && it.contains("cores") }
        assertTrue(cpu.contains("–") && !cpu.contains("|"), "an unmeasured value got a bar: $cpu")
    }

    @Test
    fun `status says when nothing has a limit and measures against the request`() {
        run("cloud status", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = true
            total = usage(3100, 8000, 12L shl 30, 24L shl 30, 4, 4)
                .setCpuLimitMillicores(0).setCpuUnlimited(true).build()
        }
        val cpu = sent.single { it.contains("CPU") && it.contains("cores") }
        assertTrue(plain(cpu).contains("no limit") && cpu.contains("|"), cpu)
    }

    @Test
    fun `status says how many pods the usage covers`() {
        run("cloud status", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = true
            total = usage(3100, 8000, 12L shl 30, 24L shl 30, 9, 8).build()
        }
        assertTrue(sent.any { it.contains("8 of 9 pods") }, "$sent")
    }

    @Test
    fun `status of a server shows its node, bars and markers`() {
        run("cloud status lobby-r", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = true
            total = usage(400, 500, 1L shl 30, 2L shl 30, 1, 1).build()
            addInstances(
                cloud.spawnery.agent.pb.InstanceStatus.newBuilder().setName("lobby-r").setGroup("lobby")
                    .setPhase("Retiring").setPlayers(3).setSlots(20).setTps(12.0).setMspt(80.0)
                    .setAgeSeconds(3 * 3600 + 20 * 60).setRetiring(true).setHeld(true).setNode("node-2")
                    .setUsage(usage(400, 500, 1L shl 30, 2L shl 30, 1, 1)),
            )
        }
        assertTrue(sent[0].contains("<bold>lobby-r</bold>") && plain(sent[0]).contains("3h20m"), sent[0])
        assertTrue(sent.any { it.contains("Node") && it.contains("node-2") }, "$sent")
        val tps = sent.single { it.contains("TPS") }
        assertTrue(tps.contains("<red>") && plain(tps).contains("12.0") && plain(tps).contains("80.0"), tps)
        assertTrue(sent.any { plain(it).contains("Players") && plain(it).contains("3 / 20") }, "$sent")
        assertTrue(sent.any { it.contains("Marked") && it.contains("retiring") && it.contains("held") }, "$sent")
    }

    @Test
    fun `status shows a server without a reported TPS as missing`() {
        run("cloud status lobby-r", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = true
            total = usage(400, 500, 1L shl 30, 2L shl 30, 1, 1).build()
            addInstances(
                cloud.spawnery.agent.pb.InstanceStatus.newBuilder().setName("lobby-r").setGroup("lobby")
                    .setPhase("Ready").setNode("node-2").setUsage(usage(400, 500, 1L shl 30, 2L shl 30, 1, 1)),
            )
        }
        val tps = sent.single { it.contains("TPS") }
        assertTrue(tps.contains("–") && !tps.contains("|"), tps)
    }

    @Test
    fun `status of an unscheduled server says so`() {
        run("cloud status lobby-r", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = true
            total = usage(0, 0, 0, 0, 0, 0).build()
            addInstances(
                cloud.spawnery.agent.pb.InstanceStatus.newBuilder().setName("lobby-r").setGroup("lobby")
                    .setPhase("Pending").setUsage(usage(0, 0, 0, 0, 0, 0)),
            )
        }
        assertTrue(sent.any { it.contains("Node") && it.contains("not scheduled") }, "$sent")
        assertTrue(sent.none { it.contains("CPU") && it.contains("|") }, "an unscheduled server got a usage bar: $sent")
    }

    @Test
    fun `status of a group lists its members with their node`() {
        run("cloud status lobby", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = true
            total = usage(400, 500, 1L shl 30, 2L shl 30, 1, 1).build()
            addGroups(
                cloud.spawnery.agent.pb.GroupStatus.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL)
                    .setPhase("Ready").setReplicas(1).setReadyReplicas(1).setPlayers(3).setLowestTps(19.5)
                    .setUsage(usage(400, 500, 1L shl 30, 2L shl 30, 1, 1)),
            )
            addInstances(
                cloud.spawnery.agent.pb.InstanceStatus.newBuilder().setName("lobby-r").setGroup("lobby")
                    .setPhase("Ready").setPlayers(3).setSlots(20).setTps(19.5).setNode("node-2")
                    .setUsage(usage(400, 500, 1L shl 30, 2L shl 30, 1, 1)),
            )
        }
        assertTrue(sent[0].contains("<bold>lobby</bold>"), sent[0])
        val member = sent.single { it.contains("lobby-r") }
        assertTrue(member.startsWith("   ") && member.contains("node-2") && member.contains("<green>19.5"), member)
    }
    @Test
    fun `one-line answers say at a glance whether it worked`() {
        run("cloud retire lobby-a")
        answer { setRetire(RetireResult.newBuilder().setServer("lobby-a")) }
        assertTrue(sent.single().startsWith("<green>✔</green> "), sent.single())
        sent.clear(); requested.clear()

        run("cloud retire lobby-a")
        answer { setError(RequestError.newBuilder().setReason(RequestError.Reason.REFUSED).setMessage("already retiring")) }
        assertTrue(sent.single().startsWith("<red>✘</red> ") && sent.single().contains("already retiring"), sent.single())
        sent.clear(); requested.clear()

        run("cloud events off")
        assertTrue(sent.single().startsWith("<green>✔</green> "), sent.single())
    }

    @Test
    fun `status of a server says why usage is missing`() {
        run("cloud status lobby-r", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = false
            total = usage(0, 500, 0, 2L shl 30, 1, 0).build()
            addInstances(
                cloud.spawnery.agent.pb.InstanceStatus.newBuilder().setName("lobby-r").setGroup("lobby")
                    .setPhase("Ready").setNode("node-2").setUsage(usage(0, 500, 0, 2L shl 30, 1, 0)),
            )
        }
        assertTrue(sent.any { it.contains("metrics API not answering") }, "$sent")
    }

    @Test
    fun `status of a group shows each member's MSPT`() {
        run("cloud status lobby", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = true
            total = usage(400, 500, 1L shl 30, 2L shl 30, 1, 1).build()
            addGroups(
                cloud.spawnery.agent.pb.GroupStatus.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL)
                    .setPhase("Ready").setReplicas(1).setReadyReplicas(1).setPlayers(3).setLowestTps(19.5)
                    .setUsage(usage(400, 500, 1L shl 30, 2L shl 30, 1, 1)),
            )
            addInstances(
                cloud.spawnery.agent.pb.InstanceStatus.newBuilder().setName("lobby-r").setGroup("lobby")
                    .setPhase("Ready").setPlayers(3).setSlots(20).setTps(19.5).setMspt(42.0).setNode("node-2")
                    .setUsage(usage(400, 500, 1L shl 30, 2L shl 30, 1, 1)),
            )
        }
        val member = plain(sent.single { it.contains("lobby-r") })
        assertTrue(member.contains("MSPT 42.0"), member)
    }

    @Test
    fun `one of a thing is counted in the singular`() {
        run("cloud status", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = true
            players = 1; servers = 1; proxies = 1
            total = usage(100, 500, 1L shl 30, 2L shl 30, 1, 1).build()
            addGroups(
                cloud.spawnery.agent.pb.GroupStatus.newBuilder().setName("arena").setKind(GroupState.Kind.EPHEMERAL)
                    .setPhase("Ready").setReplicas(1).setReadyReplicas(1).setPlayers(1)
                    .setUsage(usage(100, 500, 1L shl 30, 2L shl 30, 1, 1)),
            )
        }
        val heading = plain(sent[0])
        assertTrue(heading.contains("1 player ·") && heading.contains("1 server ·") && heading.contains("1 proxy"), heading)
        assertTrue(plain(sent.single { it.contains("arena") }).contains("1 player"), "$sent")
        assertFalse(sent.any { plain(it).contains("1 players") }, "$sent")
    }

    @Test
    fun `an on-demand group reads as words`() {
        run(
            "cloud list",
            api(
                NetworkState.newBuilder()
                    .addGroups(GroupState.newBuilder().setName("rooms").setKind(GroupState.Kind.ON_DEMAND))
                    .build(),
            ),
        )
        assertTrue(sent.any { plain(it).contains("(on-demand)") } && sent.none { it.contains("on_demand") }, "$sent")
    }

    @Test
    fun `info on a server shows playable seats beside the limit`() {
        run(
            "cloud info duels-a",
            api(
                NetworkState.newBuilder().addServers(
                    ServerState.newBuilder().setName("duels-a").setGroup("duels").setPhase("Ready")
                        .setPlayers(9).setSlots(100).setPlayableSlots(12).setRegistered(true),
                ).build(),
            ),
        )
        assertTrue(sent.any { plain(it).contains("Players") && plain(it).contains("9 / 12 · max 100") }, "$sent")
    }

    @Test
    fun `info on a group lists its servers with playable seats`() {
        run(
            "cloud info duels",
            api(
                NetworkState.newBuilder()
                    .addGroups(GroupState.newBuilder().setName("duels").setKind(GroupState.Kind.EPHEMERAL))
                    .addServers(
                        ServerState.newBuilder().setName("duels-a").setGroup("duels").setPhase("Ready")
                            .setPlayers(9).setSlots(100).setPlayableSlots(12).setRegistered(true),
                    ).build(),
            ),
        )
        assertTrue(sent.any { plain(it).contains("9/12 · max 100") }, "$sent")
    }

    @Test
    fun `status of a server with spectators shows a full bar and the limit`() {
        run("cloud status lobby-r", api(aNetworkWithProxies()))
        statusAnswer {
            metricsAvailable = true
            total = usage(400, 500, 1L shl 30, 2L shl 30, 1, 1).build()
            addInstances(
                cloud.spawnery.agent.pb.InstanceStatus.newBuilder().setName("lobby-r").setGroup("lobby")
                    .setPhase("Ready").setPlayers(14).setSlots(100).setPlayableSlots(12)
                    .setAgeSeconds(60).setUsage(usage(400, 500, 1L shl 30, 2L shl 30, 1, 1)),
            )
        }
        val players = sent.first { plain(it).contains("Players") && plain(it).contains("max") }
        assertTrue(plain(players).contains("14 / 12 · max 100"), players)
        assertTrue(players.contains("<dark_gray></dark_gray>"), "the bar is not full: $players")
    }
}

class CloudCompletionTest {
    private val adapter = object : SourceAdapter<Int> {
        override fun hasPermission(source: Int, permission: String): Boolean = true
        override fun send(source: Int, message: String) = Unit
        override fun playerId(source: Int): UUID? = null
    }

    private fun api(): SpawneryApi = MirrorApi(
        NetworkMirror().also {
            it.apply(
                NetworkState.newBuilder()
                    .addGroups(
                        GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL),
                    )
                    .addGroups(
                        GroupState.newBuilder().setName("bingo").setKind(GroupState.Kind.EPHEMERAL),
                    )
                    .addServers(
                        ServerState.newBuilder().setName("lobby-a").setGroup("lobby").setPhase("Ready"),
                    )
                    .addServers(
                        ServerState.newBuilder().setName("bingo-x").setGroup("bingo").setPhase("Ready"),
                    )
                    .build(),
            )
        },
        object : ProxySelf {
            override fun name(): String = "gateway-0"
            override fun group(): String = "gateway"
            override fun network(): String = "production"
        },
        CloudConnector(Requests(timeoutMillis = 1_000, clock = System::currentTimeMillis)) { },
        CloudEvents(),
    )

    private fun completions(command: String): List<String> {
        val dispatcher = CommandDispatcher<Int>()
        dispatcher.register(cloudCommand(api(), adapter, FeedState(), { Feed.MESSAGE_TOKEN }))
        return dispatcher.getCompletionSuggestions(dispatcher.parse(command, 0)).join().list.map { it.text }
    }

    @Test
    fun `status offers groups and servers`() {
        assertEquals(listOf("bingo", "bingo-x", "lobby", "lobby-a"), completions("cloud status ").sorted())
    }

    @Test
    fun `info offers servers and groups alike`() {
        assertEquals(listOf("bingo", "bingo-x", "lobby", "lobby-a"), completions("cloud info ").sorted())
    }

    @Test
    fun `retire offers servers and no group`() {
        val offered = completions("cloud retire ")
        assertEquals(listOf("bingo-x", "lobby-a"), offered.sorted())
    }

    @Test
    fun `start and stop offer groups and no server`() {
        assertEquals(listOf("bingo", "lobby"), completions("cloud start ").sorted())
        assertEquals(listOf("bingo", "lobby"), completions("cloud stop ").sorted())
    }

    @Test
    fun `what is already typed narrows the list`() {
        assertEquals(listOf("lobby", "lobby-a"), completions("cloud info lob").sorted())
    }

    @Test
    fun `case is ignored, because a name that came back empty reads as absent`() {
        assertEquals(listOf("lobby", "lobby-a"), completions("cloud info LOB").sorted())
    }

    @Test
    fun `a name nothing matches offers nothing rather than everything`() {
        assertEquals(emptyList(), completions("cloud info zzz"))
    }

}
