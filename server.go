package xrpc

import (
	"context"
	"encoding/json"
	"log"
	"reflect"
	"sync"
	"time"

	"github.com/edenzhong7/xrpc/pkg/encoding"

	"github.com/xtaci/smux"

	"github.com/edenzhong7/xrpc/pkg/net"

	"github.com/edenzhong7/xrpc/plugin"

	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"
)

type UnaryServerInfo = grpc.UnaryServerInfo
type UnaryServerInterceptor = grpc.UnaryServerInterceptor

type UnaryHandler func(ctx context.Context, req interface{}) (interface{}, error)

func NewServer() *Server {
	pc := plugin.NewPluginContainer()
	s := &Server{
		m:        map[string]*service{},
		mu:       &sync.Mutex{},
		lis:      map[net.Listener]bool{},
		conns:    map[net.Conn]bool{},
		sessions: map[*smux.Session]bool{},
		pc:       pc,
	}
	return s
}

// service consists of the information of the server serving this service and
// the methods in this service.
type service struct {
	server interface{} // the server for service methods
	md     map[string]*MethodDesc
	sd     map[string]*StreamDesc
	mdata  interface{}
}

type Server struct {
	opts *options

	serve  bool
	m      map[string]*service // service name -> service info
	ctx    context.Context
	cancel context.CancelFunc

	lis      map[net.Listener]bool
	conns    map[net.Conn]bool
	sessions map[*smux.Session]bool
	pc       plugin.Container

	mu       *sync.Mutex
	cv       *sync.Cond
	quit     chan struct{}
	done     chan struct{}
	quitOnce sync.Once
	doneOnce sync.Once
}

func (s *Server) Serve(lis net.Listener) error {
	go s.listen(lis)
	return nil
}

func (s *Server) SetPluginContainer(pc plugin.Container) {
	s.pc = pc
}

func (s *Server) Start() {
	for {
		time.Sleep(time.Millisecond * 100)
	}
}

func (s *Server) RegisterFunction(serviceName, fname string, fn interface{}, metadata string) {
	// TODO DoRegisterFunction
}

func (s *Server) RegisterCustomService(sd *ServiceDesc, ss interface{}) {
	// TODO DoRegisterCustomService
}

func (s *Server) RegisterService(sd *ServiceDesc, ss interface{}) {
	ht := reflect.TypeOf(sd.HandlerType).Elem()
	st := reflect.TypeOf(ss)
	if !st.Implements(ht) {
		grpclog.Fatalf("grpc: Server.RegisterService found the handler of type %v that does not satisfy %v", st, ht)
	}
	s.register(sd, ss)
}

func (s *Server) register(sd *ServiceDesc, ss interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serve {
		grpclog.Fatalf("grpc: Server.RegisterService after Server.Serve for %q", sd.ServiceName)
	}
	if _, ok := s.m[sd.ServiceName]; ok {
		grpclog.Fatalf("grpc: Server.RegisterService found duplicate service registration for %q", sd.ServiceName)
	}
	srv := &service{
		server: ss,
		md:     make(map[string]*MethodDesc),
		sd:     make(map[string]*StreamDesc),
		mdata:  sd.Metadata,
	}
	for i := range sd.Methods {
		d := &sd.Methods[i]
		srv.md[d.MethodName] = d
	}
	for i := range sd.Streams {
		d := &sd.Streams[i]
		srv.sd[d.StreamName] = d
	}
	s.m[sd.ServiceName] = srv
	// TODO DoRegister
	s.pc.DoRegisterService(sd, ss)
}

func (s *Server) listen(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			break
		}
		p := make([]byte, len(Preface))
		n, err := conn.Read(p)
		if err != nil || n != len(Preface) {
			continue
		}
		session, err := smux.Server(conn, nil)
		if err != nil {
			continue
		}
		s.sessions[session] = true
		// TODO DoConnect
		s.pc.DoConnect(conn)
		go s.handleSession(session)
	}
}

func (s *Server) handleSession(session *smux.Session) {
	log.Println("handle server session")
	defer log.Println("close server session")
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			break
		}
		pf, data, err := recv(stream)
		if err != nil {
			return
		}
		if pf == CmdHeader {
			header := &streamHeader{}
			err = json.Unmarshal(data, header)
			if err != nil {
				continue
			}
			ss := &serverStream{
				stream: &streamConn{stream},
				codec:  encoding.GetCodec(getCodecArg(header)),
				cp:     encoding.GetCompressor(getCompressorArg(header)),
			}
			ss.header = header
			// TODO DoOpenStream
			if _, err = s.pc.DoOpenStream(context.Background(), stream); err != nil {
				continue
			}
			go s.processStream(s.ctx, ss, header)
		}
	}
	// TODO DoDisconnect
	s.pc.DoDisconnect(nil)
}

func (s *Server) processStream(ctx context.Context, stream ServerStream, header *streamHeader) {
	log.Println("process server stream")
	log.Println("close server stream")
	service, method := header.splitMethod()
	if service == "" || method == "" {
		return
	}
	srv := s.m[service].server
	desc := s.m[service].md[method]
	for {
		newCtx := ctx
		var err error
		reply, err := desc.Handler(srv, newCtx, stream.RecvMsg, s.pc.DoHandle)
		if err != nil {
			break
		}
		if err = stream.SendMsg(reply); err != nil {
			break
		}
	}
	// TODO DoCloseStream
	s.pc.DoCloseStream(ctx, stream.(*serverStream).stream)
}
