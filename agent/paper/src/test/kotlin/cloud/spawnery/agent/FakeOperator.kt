package cloud.spawnery.agent

import cloud.spawnery.agent.pb.AgentServiceGrpc
import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.ServerMessage
import io.grpc.Context
import io.grpc.Contexts
import io.grpc.Metadata
import io.grpc.Server
import io.grpc.ServerCall
import io.grpc.ServerCallHandler
import io.grpc.ServerInterceptor
import io.grpc.inprocess.InProcessChannelBuilder
import io.grpc.inprocess.InProcessServerBuilder
import io.grpc.stub.StreamObserver
import java.util.concurrent.ConcurrentLinkedQueue
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/** One accepted stream, with everything the test wants to assert about it. */
class AcceptedStream(val authorization: String?) {
    val received = ConcurrentLinkedQueue<ServerMessage>()
    val closed = CountDownLatch(1)
    lateinit var toAgent: StreamObserver<OperatorToServer>

    fun awaitMessage(predicate: (ServerMessage) -> Boolean): ServerMessage {
        val deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5)
        while (System.nanoTime() < deadline) {
            received.firstOrNull(predicate)?.let { return it }
            Thread.sleep(10)
        }
        throw AssertionError("no matching message within 5s; saw: ${received.toList()}")
    }
}

/**
 * An in-process operator. It is not a mock of the real one — it only has to
 * accept a stream and record it, because what is under test is the agent's half
 * of the conversation.
 */
class FakeOperator(name: String) : AutoCloseable {
    val streams = ConcurrentLinkedQueue<AcceptedStream>()
    private val header = Metadata.Key.of("authorization", Metadata.ASCII_STRING_MARSHALLER)

    // A gRPC Context key, not a ThreadLocal: for a bidi-streaming call the
    // handler method (serverSession, below) runs synchronously inside
    // ServerCallHandler.startCall to obtain the request StreamObserver, which
    // is itself invoked from inside interceptCall's next.startCall(). Routing
    // the header through Contexts.interceptCall attaches it to the Context for
    // exactly that scope, so the read is correct regardless of which executor
    // or thread actually runs the call — unlike a ThreadLocal, which is only
    // correct if interception and dispatch happen to share a thread.
    private val authorizationKey = Context.key<String?>("authorization")

    private val service = object : AgentServiceGrpc.AgentServiceImplBase() {
        override fun serverSession(
            responseObserver: StreamObserver<OperatorToServer>,
        ): StreamObserver<ServerMessage> {
            val accepted = AcceptedStream(authorizationKey.get())
            accepted.toAgent = responseObserver
            streams.add(accepted)
            return object : StreamObserver<ServerMessage> {
                override fun onNext(value: ServerMessage) { accepted.received.add(value) }
                override fun onError(t: Throwable) { accepted.closed.countDown() }
                override fun onCompleted() { accepted.closed.countDown() }
            }
        }
    }

    private val recorder = object : ServerInterceptor {
        override fun <Q : Any, S : Any> interceptCall(
            call: ServerCall<Q, S>,
            headers: Metadata,
            next: ServerCallHandler<Q, S>,
        ): ServerCall.Listener<Q> {
            val context = Context.current().withValue(authorizationKey, headers.get(header))
            return Contexts.interceptCall(context, call, headers, next)
        }
    }

    private val server: Server = InProcessServerBuilder.forName(name)
        .directExecutor()
        .addService(io.grpc.ServerInterceptors.intercept(service, recorder))
        .build()
        .start()

    val channelName = name

    fun awaitStream(index: Int): AcceptedStream {
        val deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5)
        while (System.nanoTime() < deadline) {
            streams.toList().getOrNull(index)?.let { return it }
            Thread.sleep(10)
        }
        throw AssertionError("stream $index never arrived; have ${streams.size}")
    }

    fun newChannel() = InProcessChannelBuilder.forName(channelName).directExecutor().build()

    override fun close() {
        server.shutdownNow()
        server.awaitTermination(5, TimeUnit.SECONDS)
    }
}
