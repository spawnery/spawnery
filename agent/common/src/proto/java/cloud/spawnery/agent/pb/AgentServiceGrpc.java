package cloud.spawnery.agent.pb;

import static io.grpc.MethodDescriptor.generateFullMethodName;

/**
 * <pre>
 * AgentService is the only channel between the operator and the in-game
 * agents. The agents never read the Kubernetes API; this stream carries both
 * directions instead.
 * </pre>
 */
@io.grpc.stub.annotations.GrpcGenerated
public final class AgentServiceGrpc {

  private AgentServiceGrpc() {}

  public static final java.lang.String SERVICE_NAME = "spawnery.agent.v1alpha1.AgentService";

  // Static method descriptors that strictly reflect the proto.
  private static volatile io.grpc.MethodDescriptor<cloud.spawnery.agent.pb.ProxyMessage,
      cloud.spawnery.agent.pb.OperatorToProxy> getProxySessionMethod;

  @io.grpc.stub.annotations.RpcMethod(
      fullMethodName = SERVICE_NAME + '/' + "ProxySession",
      requestType = cloud.spawnery.agent.pb.ProxyMessage.class,
      responseType = cloud.spawnery.agent.pb.OperatorToProxy.class,
      methodType = io.grpc.MethodDescriptor.MethodType.BIDI_STREAMING)
  public static io.grpc.MethodDescriptor<cloud.spawnery.agent.pb.ProxyMessage,
      cloud.spawnery.agent.pb.OperatorToProxy> getProxySessionMethod() {
    io.grpc.MethodDescriptor<cloud.spawnery.agent.pb.ProxyMessage, cloud.spawnery.agent.pb.OperatorToProxy> getProxySessionMethod;
    if ((getProxySessionMethod = AgentServiceGrpc.getProxySessionMethod) == null) {
      synchronized (AgentServiceGrpc.class) {
        if ((getProxySessionMethod = AgentServiceGrpc.getProxySessionMethod) == null) {
          AgentServiceGrpc.getProxySessionMethod = getProxySessionMethod =
              io.grpc.MethodDescriptor.<cloud.spawnery.agent.pb.ProxyMessage, cloud.spawnery.agent.pb.OperatorToProxy>newBuilder()
              .setType(io.grpc.MethodDescriptor.MethodType.BIDI_STREAMING)
              .setFullMethodName(generateFullMethodName(SERVICE_NAME, "ProxySession"))
              .setSampledToLocalTracing(true)
              .setRequestMarshaller(io.grpc.protobuf.ProtoUtils.marshaller(
                  cloud.spawnery.agent.pb.ProxyMessage.getDefaultInstance()))
              .setResponseMarshaller(io.grpc.protobuf.ProtoUtils.marshaller(
                  cloud.spawnery.agent.pb.OperatorToProxy.getDefaultInstance()))
              .setSchemaDescriptor(new AgentServiceMethodDescriptorSupplier("ProxySession"))
              .build();
        }
      }
    }
    return getProxySessionMethod;
  }

  private static volatile io.grpc.MethodDescriptor<cloud.spawnery.agent.pb.ServerMessage,
      cloud.spawnery.agent.pb.OperatorToServer> getServerSessionMethod;

  @io.grpc.stub.annotations.RpcMethod(
      fullMethodName = SERVICE_NAME + '/' + "ServerSession",
      requestType = cloud.spawnery.agent.pb.ServerMessage.class,
      responseType = cloud.spawnery.agent.pb.OperatorToServer.class,
      methodType = io.grpc.MethodDescriptor.MethodType.BIDI_STREAMING)
  public static io.grpc.MethodDescriptor<cloud.spawnery.agent.pb.ServerMessage,
      cloud.spawnery.agent.pb.OperatorToServer> getServerSessionMethod() {
    io.grpc.MethodDescriptor<cloud.spawnery.agent.pb.ServerMessage, cloud.spawnery.agent.pb.OperatorToServer> getServerSessionMethod;
    if ((getServerSessionMethod = AgentServiceGrpc.getServerSessionMethod) == null) {
      synchronized (AgentServiceGrpc.class) {
        if ((getServerSessionMethod = AgentServiceGrpc.getServerSessionMethod) == null) {
          AgentServiceGrpc.getServerSessionMethod = getServerSessionMethod =
              io.grpc.MethodDescriptor.<cloud.spawnery.agent.pb.ServerMessage, cloud.spawnery.agent.pb.OperatorToServer>newBuilder()
              .setType(io.grpc.MethodDescriptor.MethodType.BIDI_STREAMING)
              .setFullMethodName(generateFullMethodName(SERVICE_NAME, "ServerSession"))
              .setSampledToLocalTracing(true)
              .setRequestMarshaller(io.grpc.protobuf.ProtoUtils.marshaller(
                  cloud.spawnery.agent.pb.ServerMessage.getDefaultInstance()))
              .setResponseMarshaller(io.grpc.protobuf.ProtoUtils.marshaller(
                  cloud.spawnery.agent.pb.OperatorToServer.getDefaultInstance()))
              .setSchemaDescriptor(new AgentServiceMethodDescriptorSupplier("ServerSession"))
              .build();
        }
      }
    }
    return getServerSessionMethod;
  }

  /**
   * Creates a new async stub that supports all call types for the service
   */
  public static AgentServiceStub newStub(io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<AgentServiceStub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<AgentServiceStub>() {
        @java.lang.Override
        public AgentServiceStub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new AgentServiceStub(channel, callOptions);
        }
      };
    return AgentServiceStub.newStub(factory, channel);
  }

  /**
   * Creates a new blocking-style stub that supports all types of calls on the service
   */
  public static AgentServiceBlockingV2Stub newBlockingV2Stub(
      io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<AgentServiceBlockingV2Stub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<AgentServiceBlockingV2Stub>() {
        @java.lang.Override
        public AgentServiceBlockingV2Stub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new AgentServiceBlockingV2Stub(channel, callOptions);
        }
      };
    return AgentServiceBlockingV2Stub.newStub(factory, channel);
  }

  /**
   * Creates a new blocking-style stub that supports unary and streaming output calls on the service
   */
  public static AgentServiceBlockingStub newBlockingStub(
      io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<AgentServiceBlockingStub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<AgentServiceBlockingStub>() {
        @java.lang.Override
        public AgentServiceBlockingStub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new AgentServiceBlockingStub(channel, callOptions);
        }
      };
    return AgentServiceBlockingStub.newStub(factory, channel);
  }

  /**
   * Creates a new ListenableFuture-style stub that supports unary calls on the service
   */
  public static AgentServiceFutureStub newFutureStub(
      io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<AgentServiceFutureStub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<AgentServiceFutureStub>() {
        @java.lang.Override
        public AgentServiceFutureStub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new AgentServiceFutureStub(channel, callOptions);
        }
      };
    return AgentServiceFutureStub.newStub(factory, channel);
  }

  /**
   * <pre>
   * AgentService is the only channel between the operator and the in-game
   * agents. The agents never read the Kubernetes API; this stream carries both
   * directions instead.
   * </pre>
   */
  public interface AsyncService {

    /**
     * <pre>
     * ProxySession is the Velocity agent's channel.
     * </pre>
     */
    default io.grpc.stub.StreamObserver<cloud.spawnery.agent.pb.ProxyMessage> proxySession(
        io.grpc.stub.StreamObserver<cloud.spawnery.agent.pb.OperatorToProxy> responseObserver) {
      return io.grpc.stub.ServerCalls.asyncUnimplementedStreamingCall(getProxySessionMethod(), responseObserver);
    }

    /**
     * <pre>
     * ServerSession is the Paper agent's channel.
     * </pre>
     */
    default io.grpc.stub.StreamObserver<cloud.spawnery.agent.pb.ServerMessage> serverSession(
        io.grpc.stub.StreamObserver<cloud.spawnery.agent.pb.OperatorToServer> responseObserver) {
      return io.grpc.stub.ServerCalls.asyncUnimplementedStreamingCall(getServerSessionMethod(), responseObserver);
    }
  }

  /**
   * Base class for the server implementation of the service AgentService.
   * <pre>
   * AgentService is the only channel between the operator and the in-game
   * agents. The agents never read the Kubernetes API; this stream carries both
   * directions instead.
   * </pre>
   */
  public static abstract class AgentServiceImplBase
      implements io.grpc.BindableService, AsyncService {

    @java.lang.Override public final io.grpc.ServerServiceDefinition bindService() {
      return AgentServiceGrpc.bindService(this);
    }
  }

  /**
   * A stub to allow clients to do asynchronous rpc calls to service AgentService.
   * <pre>
   * AgentService is the only channel between the operator and the in-game
   * agents. The agents never read the Kubernetes API; this stream carries both
   * directions instead.
   * </pre>
   */
  public static final class AgentServiceStub
      extends io.grpc.stub.AbstractAsyncStub<AgentServiceStub> {
    private AgentServiceStub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected AgentServiceStub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new AgentServiceStub(channel, callOptions);
    }

    /**
     * <pre>
     * ProxySession is the Velocity agent's channel.
     * </pre>
     */
    public io.grpc.stub.StreamObserver<cloud.spawnery.agent.pb.ProxyMessage> proxySession(
        io.grpc.stub.StreamObserver<cloud.spawnery.agent.pb.OperatorToProxy> responseObserver) {
      return io.grpc.stub.ClientCalls.asyncBidiStreamingCall(
          getChannel().newCall(getProxySessionMethod(), getCallOptions()), responseObserver);
    }

    /**
     * <pre>
     * ServerSession is the Paper agent's channel.
     * </pre>
     */
    public io.grpc.stub.StreamObserver<cloud.spawnery.agent.pb.ServerMessage> serverSession(
        io.grpc.stub.StreamObserver<cloud.spawnery.agent.pb.OperatorToServer> responseObserver) {
      return io.grpc.stub.ClientCalls.asyncBidiStreamingCall(
          getChannel().newCall(getServerSessionMethod(), getCallOptions()), responseObserver);
    }
  }

  /**
   * A stub to allow clients to do synchronous rpc calls to service AgentService.
   * <pre>
   * AgentService is the only channel between the operator and the in-game
   * agents. The agents never read the Kubernetes API; this stream carries both
   * directions instead.
   * </pre>
   */
  public static final class AgentServiceBlockingV2Stub
      extends io.grpc.stub.AbstractBlockingStub<AgentServiceBlockingV2Stub> {
    private AgentServiceBlockingV2Stub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected AgentServiceBlockingV2Stub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new AgentServiceBlockingV2Stub(channel, callOptions);
    }

    /**
     * <pre>
     * ProxySession is the Velocity agent's channel.
     * </pre>
     */
    @io.grpc.ExperimentalApi("https://github.com/grpc/grpc-java/issues/10918")
    public io.grpc.stub.BlockingClientCall<cloud.spawnery.agent.pb.ProxyMessage, cloud.spawnery.agent.pb.OperatorToProxy>
        proxySession() {
      return io.grpc.stub.ClientCalls.blockingBidiStreamingCall(
          getChannel(), getProxySessionMethod(), getCallOptions());
    }

    /**
     * <pre>
     * ServerSession is the Paper agent's channel.
     * </pre>
     */
    @io.grpc.ExperimentalApi("https://github.com/grpc/grpc-java/issues/10918")
    public io.grpc.stub.BlockingClientCall<cloud.spawnery.agent.pb.ServerMessage, cloud.spawnery.agent.pb.OperatorToServer>
        serverSession() {
      return io.grpc.stub.ClientCalls.blockingBidiStreamingCall(
          getChannel(), getServerSessionMethod(), getCallOptions());
    }
  }

  /**
   * A stub to allow clients to do limited synchronous rpc calls to service AgentService.
   * <pre>
   * AgentService is the only channel between the operator and the in-game
   * agents. The agents never read the Kubernetes API; this stream carries both
   * directions instead.
   * </pre>
   */
  public static final class AgentServiceBlockingStub
      extends io.grpc.stub.AbstractBlockingStub<AgentServiceBlockingStub> {
    private AgentServiceBlockingStub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected AgentServiceBlockingStub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new AgentServiceBlockingStub(channel, callOptions);
    }
  }

  /**
   * A stub to allow clients to do ListenableFuture-style rpc calls to service AgentService.
   * <pre>
   * AgentService is the only channel between the operator and the in-game
   * agents. The agents never read the Kubernetes API; this stream carries both
   * directions instead.
   * </pre>
   */
  public static final class AgentServiceFutureStub
      extends io.grpc.stub.AbstractFutureStub<AgentServiceFutureStub> {
    private AgentServiceFutureStub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected AgentServiceFutureStub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new AgentServiceFutureStub(channel, callOptions);
    }
  }

  private static final int METHODID_PROXY_SESSION = 0;
  private static final int METHODID_SERVER_SESSION = 1;

  private static final class MethodHandlers<Req, Resp> implements
      io.grpc.stub.ServerCalls.UnaryMethod<Req, Resp>,
      io.grpc.stub.ServerCalls.ServerStreamingMethod<Req, Resp>,
      io.grpc.stub.ServerCalls.ClientStreamingMethod<Req, Resp>,
      io.grpc.stub.ServerCalls.BidiStreamingMethod<Req, Resp> {
    private final AsyncService serviceImpl;
    private final int methodId;

    MethodHandlers(AsyncService serviceImpl, int methodId) {
      this.serviceImpl = serviceImpl;
      this.methodId = methodId;
    }

    @java.lang.Override
    @java.lang.SuppressWarnings("unchecked")
    public void invoke(Req request, io.grpc.stub.StreamObserver<Resp> responseObserver) {
      switch (methodId) {
        default:
          throw new AssertionError();
      }
    }

    @java.lang.Override
    @java.lang.SuppressWarnings("unchecked")
    public io.grpc.stub.StreamObserver<Req> invoke(
        io.grpc.stub.StreamObserver<Resp> responseObserver) {
      switch (methodId) {
        case METHODID_PROXY_SESSION:
          return (io.grpc.stub.StreamObserver<Req>) serviceImpl.proxySession(
              (io.grpc.stub.StreamObserver<cloud.spawnery.agent.pb.OperatorToProxy>) responseObserver);
        case METHODID_SERVER_SESSION:
          return (io.grpc.stub.StreamObserver<Req>) serviceImpl.serverSession(
              (io.grpc.stub.StreamObserver<cloud.spawnery.agent.pb.OperatorToServer>) responseObserver);
        default:
          throw new AssertionError();
      }
    }
  }

  public static final io.grpc.ServerServiceDefinition bindService(AsyncService service) {
    return io.grpc.ServerServiceDefinition.builder(getServiceDescriptor())
        .addMethod(
          getProxySessionMethod(),
          io.grpc.stub.ServerCalls.asyncBidiStreamingCall(
            new MethodHandlers<
              cloud.spawnery.agent.pb.ProxyMessage,
              cloud.spawnery.agent.pb.OperatorToProxy>(
                service, METHODID_PROXY_SESSION)))
        .addMethod(
          getServerSessionMethod(),
          io.grpc.stub.ServerCalls.asyncBidiStreamingCall(
            new MethodHandlers<
              cloud.spawnery.agent.pb.ServerMessage,
              cloud.spawnery.agent.pb.OperatorToServer>(
                service, METHODID_SERVER_SESSION)))
        .build();
  }

  private static abstract class AgentServiceBaseDescriptorSupplier
      implements io.grpc.protobuf.ProtoFileDescriptorSupplier, io.grpc.protobuf.ProtoServiceDescriptorSupplier {
    AgentServiceBaseDescriptorSupplier() {}

    @java.lang.Override
    public com.google.protobuf.Descriptors.FileDescriptor getFileDescriptor() {
      return cloud.spawnery.agent.pb.AgentProto.getDescriptor();
    }

    @java.lang.Override
    public com.google.protobuf.Descriptors.ServiceDescriptor getServiceDescriptor() {
      return getFileDescriptor().findServiceByName("AgentService");
    }
  }

  private static final class AgentServiceFileDescriptorSupplier
      extends AgentServiceBaseDescriptorSupplier {
    AgentServiceFileDescriptorSupplier() {}
  }

  private static final class AgentServiceMethodDescriptorSupplier
      extends AgentServiceBaseDescriptorSupplier
      implements io.grpc.protobuf.ProtoMethodDescriptorSupplier {
    private final java.lang.String methodName;

    AgentServiceMethodDescriptorSupplier(java.lang.String methodName) {
      this.methodName = methodName;
    }

    @java.lang.Override
    public com.google.protobuf.Descriptors.MethodDescriptor getMethodDescriptor() {
      return getServiceDescriptor().findMethodByName(methodName);
    }
  }

  private static volatile io.grpc.ServiceDescriptor serviceDescriptor;

  public static io.grpc.ServiceDescriptor getServiceDescriptor() {
    io.grpc.ServiceDescriptor result = serviceDescriptor;
    if (result == null) {
      synchronized (AgentServiceGrpc.class) {
        result = serviceDescriptor;
        if (result == null) {
          serviceDescriptor = result = io.grpc.ServiceDescriptor.newBuilder(SERVICE_NAME)
              .setSchemaDescriptor(new AgentServiceFileDescriptorSupplier())
              .addMethod(getProxySessionMethod())
              .addMethod(getServerSessionMethod())
              .build();
        }
      }
    }
    return result;
  }
}
