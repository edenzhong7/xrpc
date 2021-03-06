package xrpc

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"time"

	"x.io/xrpc/pkg/encoding"
	"x.io/xrpc/pkg/log"
	"x.io/xrpc/pkg/net"
	"x.io/xrpc/plugin"
	"x.io/xrpc/types"

	"github.com/xtaci/smux"
)

func NewServer() *Server {
	pc := plugin.NewPluginContainer()
	s := &Server{
		CustomServer: NewCustomServer(pc),
		m:            map[string]*service{},
		mu:           &sync.Mutex{},
		lis:          map[net.Listener]bool{},
		conns:        map[net.Conn]bool{},
		sessions:     map[*smux.Session]bool{},
		pc:           pc,
		ctx:          context.Background(),
		auth:         NewEmptyAuthenticator(),
	}
	return s
}

// service consists of the information of the server serving this service and
// the methods in this service.
type service struct {
	server interface{} // the server for service methods
	md     map[string]*types.MethodDesc
	sd     map[string]*types.StreamDesc
	mdata  interface{}
}

type Server struct {
	*CustomServer
	opts *options

	serve  bool
	m      map[string]*service // service name -> service info
	ctx    context.Context
	cancel context.CancelFunc

	auth Authenticator

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
	err := s.listen(lis)
	return err
}

func (s *Server) SetAuthenticator(authenticator Authenticator) {
	s.auth = authenticator
}

func (s *Server) Shutdown() (err error) {
	if s.pc != nil {
		err = s.pc.Stop()
	}
	return
}

func (s *Server) StartPlugins() (err error) {
	if s.pc != nil {
		err = s.pc.Start()
	}
	return
}

func (s *Server) ApplyPlugins(plugins ...plugin.Plugin) {
	for _, p := range plugins {
		s.pc.Add(p)
	}
}

func (s *Server) Start() {
	for {
		time.Sleep(time.Millisecond * 100)
	}
}

func (s *Server) RegisterService(sd *types.ServiceDesc, ss interface{}) {
	ht := reflect.TypeOf(sd.HandlerType).Elem()
	st := reflect.TypeOf(ss)
	if !st.Implements(ht) {
		log.Fatalf("xrpc: Server.RegisterCustomService found the handler of type %v that does not satisfy %v", st, ht)
	}
	s.register(sd, ss)
}

func (s *Server) register(sd *types.ServiceDesc, ss interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serve {
		log.Fatalf("xrpc: Server.RegisterCustomService after Server.Serve for %q", sd.ServiceName)
	}
	if _, ok := s.m[sd.ServiceName]; ok {
		log.Fatalf("xrpc: Server.RegisterCustomService found duplicate service registration for %q", sd.ServiceName)
	}
	srv := &service{
		server: ss,
		md:     make(map[string]*types.MethodDesc),
		sd:     make(map[string]*types.StreamDesc),
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
	// DoRegister
	s.pc.DoRegisterService(sd, ss)
}

func (s *Server) listen(lis net.Listener) (e error) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			e = err
			break
		}
		p := make([]byte, len(types.Preface))
		n, err := conn.Read(p)
		if err != nil || n != len(types.Preface) {
			continue
		}
		session, err := smux.Server(conn, nil)
		if err != nil {
			continue
		}
		s.sessions[session] = true
		// DoConnect
		conn, ok := s.pc.DoConnect(conn)
		if !ok {
			conn.Close()
			continue
		}
		go s.handleSession(conn, session)
	}
	return
}

func (s *Server) handleSession(conn net.Conn, session *smux.Session) {
	log.Debug("handle server session")
	defer log.Debug("close server session")
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			break
		}
		pf, data, err := recv(stream)
		if err != nil {
			return
		}
		if pf == types.CmdHeader {
			header := &types.StreamHeader{}
			err = json.Unmarshal(data, header)
			if err != nil {
				continue
			}
			if err = s.auth.Authenticate(header.Args); err != nil {
				log.Error(err.Error())
				stream.Close()
				continue
			}
			ss := &serverStream{
				stream: &streamConn{stream},
				codec:  encoding.GetCodec(types.GetCodecArg(header)),
				cp:     encoding.GetCompressor(types.GetCompressorArg(header)),
				sc:     s.pc,
				header: header,
			}

			ctx := context.Background()
			// DoOpenStream
			if ctx, err = s.pc.DoOpenStream(ctx, stream); err != nil {
				continue
			}

			for k, v := range header.Args {
				if vv, ok := v.(string); ok {
					ctx = types.SetCookie(ctx, k, vv)
				}
			}
			go s.processStream(ctx, ss, header)
		}
	}
	// DoDisconnect
	s.pc.DoDisconnect(conn)
}

func (s *Server) processStream(ctx context.Context, stream types.ServerStream, header *types.StreamHeader) {
	defer s.pc.DoCloseStream(ctx, stream.(*serverStream).stream)
	service, method := header.SplitMethod()
	if service == "" || method == "" {
		return
	}
	if header.RpcType == types.RawRPC {
		// RawRPC
		var newCtx context.Context

		dec := func(m interface{}) (err error) {
			newCtx, err = stream.RecvMsg(newCtx, m)
			return
		}
		for {
			newCtx = ctx
			reply, err := s.RpcCall(newCtx, service, method, dec, s.pc.DoIntercept)
			if err != nil {
				break
			}
			if res, ok := reply.([]interface{}); ok {
				if len(res) == 1 {
					reply = res[0]
				}
			}
			if err = stream.SendMsg(newCtx, reply); err != nil {
				break
			}
		}
		return
	}

	// XRPC
	srv := s.m[service].server
	desc := s.m[service].md[method]
	var newCtx context.Context

	dec := func(m interface{}) (err error) {
		newCtx, err = stream.RecvMsg(newCtx, m)
		return
	}
	for {
		newCtx = ctx
		reply, err := desc.Handler(srv, newCtx, dec, s.pc.DoIntercept)
		if err != nil {
			break
		}
		if err = stream.SendMsg(newCtx, reply); err != nil {
			break
		}
	}
}
