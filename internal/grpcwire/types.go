package grpcwire

import (
	"context"

	"github.com/ztelliot/mtr/internal/model"
	"google.golang.org/grpc"
)

const ServiceName = "mtr.AgentControl"

type AgentMessage struct {
	Type      string       `json:"type"`
	Agent     *AgentHello  `json:"agent,omitempty"`
	Heartbeat *Heartbeat   `json:"heartbeat,omitempty"`
	Result    *AgentResult `json:"result,omitempty"`
}

type ServerMessage struct {
	Type   string   `json:"type"`
	Job    *JobSpec `json:"job,omitempty"`
	Cancel *Cancel  `json:"cancel,omitempty"`
	Error  string   `json:"error,omitempty"`
}

type AgentHello struct {
	ID           string             `json:"id"`
	Country      string             `json:"country,omitempty"`
	Region       string             `json:"region"`
	Provider     string             `json:"provider,omitempty"`
	ISP          string             `json:"isp,omitempty"`
	Version      string             `json:"version,omitempty"`
	Token        string             `json:"token"`
	Capabilities []model.Tool       `json:"capabilities"`
	Protocols    model.ProtocolMask `json:"protocols"`
}

type Heartbeat struct{}

type JobSpec struct {
	ID                    string            `json:"id"`
	Tool                  model.Tool        `json:"tool"`
	Target                string            `json:"target"`
	ResolvedTarget        string            `json:"resolved_target,omitempty"`
	Args                  map[string]string `json:"args,omitempty"`
	IPVersion             model.IPVersion   `json:"ip_version,omitempty"`
	ResolveOnAgent        bool              `json:"resolve_on_agent,omitempty"`
	TimeoutSeconds        int               `json:"timeout_seconds"`
	ProbeTimeoutSeconds   int               `json:"probe_timeout_seconds,omitempty"`
	ResolveTimeoutSeconds int               `json:"resolve_timeout_seconds,omitempty"`
}

type ResultEvent struct {
	JobID   string         `json:"job_id"`
	AgentID string         `json:"agent_id"`
	Event   map[string]any `json:"event"`
}

type AgentResult struct {
	JobID string         `json:"job_id"`
	Event map[string]any `json:"event"`
}

type Cancel struct {
	JobID string `json:"job_id"`
}

type ControlServer interface {
	Connect(Control_ConnectServer) error
}

type ControlClient interface {
	Connect(ctx context.Context, opts ...grpc.CallOption) (Control_ConnectClient, error)
}

type controlClient struct {
	cc grpc.ClientConnInterface
}

func NewControlClient(cc grpc.ClientConnInterface) ControlClient {
	return &controlClient{cc: cc}
}

func (c *controlClient) Connect(ctx context.Context, opts ...grpc.CallOption) (Control_ConnectClient, error) {
	stream, err := c.cc.NewStream(ctx, &Control_ServiceDesc.Streams[0], "/"+ServiceName+"/Connect", opts...)
	if err != nil {
		return nil, err
	}
	return &controlConnectClient{ClientStream: stream}, nil
}

type Control_ConnectClient interface {
	Send(*AgentMessage) error
	Recv() (*ServerMessage, error)
	grpc.ClientStream
}

type controlConnectClient struct {
	grpc.ClientStream
}

func (x *controlConnectClient) Send(m *AgentMessage) error {
	return x.ClientStream.SendMsg(m)
}

func (x *controlConnectClient) Recv() (*ServerMessage, error) {
	m := new(ServerMessage)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

type Control_ConnectServer interface {
	Send(*ServerMessage) error
	Recv() (*AgentMessage, error)
	grpc.ServerStream
}

type controlConnectServer struct {
	grpc.ServerStream
}

func (x *controlConnectServer) Send(m *ServerMessage) error {
	return x.ServerStream.SendMsg(m)
}

func (x *controlConnectServer) Recv() (*AgentMessage, error) {
	m := new(AgentMessage)
	if err := x.ServerStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func RegisterControlServer(s grpc.ServiceRegistrar, srv ControlServer) {
	s.RegisterService(&Control_ServiceDesc, srv)
}

var Control_ServiceDesc = grpc.ServiceDesc{
	ServiceName: ServiceName,
	HandlerType: (*ControlServer)(nil),
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "Connect",
			Handler:       _Control_Connect_Handler,
			ServerStreams: true,
			ClientStreams: true,
		},
	},
	Metadata: "mtr.json",
}

func _Control_Connect_Handler(srv any, stream grpc.ServerStream) error {
	return srv.(ControlServer).Connect(&controlConnectServer{ServerStream: stream})
}
