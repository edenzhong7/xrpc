package chord

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"x.io/xrpc"
	"x.io/xrpc/pkg/crypto"
	_ "x.io/xrpc/pkg/encoding/gzip"
	_ "x.io/xrpc/pkg/encoding/json"
	_ "x.io/xrpc/pkg/encoding/proto"
	_ "x.io/xrpc/pkg/encoding/snappy"
	"x.io/xrpc/pkg/log"
	cryptop "x.io/xrpc/plugin/crypto"

	"x.io/xrpc/protocol/chordpb"
)

func NewChord(host string, port int, h chordpb.Hasher, store chordpb.KVStore) *chordImpl {
	addr := fmt.Sprintf("%s:%d", host, port)
	var id chordpb.NodeID = h.Hash([]byte(addr))
	c := &chordImpl{
		host:          host,
		port:          port,
		id:            id,
		self:          &chordpb.Node{Id: id, Host: host, Port: port},
		remoteNodes:   map[chordpb.NodeID]chordpb.ChordClient{},
		activeRecords: map[chordpb.NodeID]time.Time{},
		conns:         map[chordpb.NodeID]*xrpc.ClientConn{},
		amu:           &sync.Mutex{},

		h:     h,
		store: store,
	}
	size := h.Size()
	c.fingerTable = make([]*Finger, h.Size(), h.Size())
	for i := 0; i < size; i++ {
		f := &Finger{
			index: i,
			id:    id.Add(big.NewInt(2 << (i + 1))),
			node:  *(c.self),
		}
		c.fingerTable[i] = f
	}
	return c
}

type Finger struct {
	index int
	id    chordpb.NodeID
	node  chordpb.Node
}

func NewChordAPI(ci *chordImpl) HttpAPI {
	return &chordAPI{ci: ci}
}

type chordAPI struct {
	ci *chordImpl
}

func (c chordAPI) set(key, value string) {
	var id chordpb.NodeID = c.ci.h.Hash([]byte(key))
	req := c.ci.NewMessage(chordpb.KeySet, id, []byte(key), []byte(value))
	ctx := c.ci.newXCtx()
	c.ci.Set(ctx, req)
}

func (c chordAPI) get(key string) string {
	var id chordpb.NodeID = c.ci.h.Hash([]byte(key))
	req := c.ci.NewMessage(chordpb.KeyGet, id, []byte(key), nil)
	ctx := c.ci.newXCtx()
	reply := c.ci.Get(ctx, req)
	if reply.Purpose != chordpb.StatusOk {
		return ""
	}
	return string(reply.Body)
}

func (c chordAPI) del(key string) {
	var id chordpb.NodeID = c.ci.h.Hash([]byte(key))
	req := c.ci.NewMessage(chordpb.KeyDel, id, []byte(key), nil)
	ctx := c.ci.newXCtx()
	c.ci.Del(ctx, req)
}

type chordImpl struct {
	host string
	port int
	id   chordpb.NodeID
	self *chordpb.Node

	remoteNodes   map[chordpb.NodeID]chordpb.ChordClient
	activeRecords map[chordpb.NodeID]time.Time
	conns         map[chordpb.NodeID]*xrpc.ClientConn

	amu *sync.Mutex

	successor   *chordpb.Node
	predecessor *chordpb.Node
	fingerTable []*Finger // 2^(1,2,...,128)

	store chordpb.KVStore
	h     chordpb.Hasher

	quit chan struct{}
}

func (c *chordImpl) Join(ctx *xrpc.XContext, req *chordpb.Message) (reply *chordpb.Message) {
	id := req.ID
	finger, ok := c.findFinger(id)
	senderID := req.Sender.Id

	reply = c.NewMessage(chordpb.StatusOk, senderID, nil, nil)
	reply.ID = req.Sender.Id
	reply.Target = req.Sender
	if !ok {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr(fmt.Sprintf("can't find finger for %s at node: %s", id.String(), c.id.String())))
		return
	}

	if c.predecessor == nil || (c.predecessor.Id.Less(senderID) && senderID.Less(c.id)) {
		if c.successor == nil || c.successor.Id == c.id {
			c.successor = &(req.Sender)
			c.notify()
		}
		return
	}

	next, err := c.checkNode(finger)
	if err != nil {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr("can't find remote node by id: "+finger.Id.String()))
		return
	}
	reply = next.Join(ctx, req)
	return
}

func (c *chordImpl) Leave(ctx *xrpc.XContext, req *chordpb.Message) (reply *chordpb.Message) {
	id := req.ID
	finger, ok := c.findFinger(id)

	reply = c.NewMessage(chordpb.StatusOk, req.Sender.Id, nil, nil)
	reply.ID = req.Sender.Id
	reply.Target = req.Sender
	if !ok {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr(fmt.Sprintf("can't find finger for %s at node: %s", id.String(), c.id.String())))
		return
	}
	if finger.Id == c.id {
		if c.successor != nil && c.successor.Id.Equal(req.Sender.Id) {
			newSuccessor := chordpb.Node{}
			err := json.Unmarshal(req.Body, &newSuccessor)
			if err != nil {
				reply.Purpose = chordpb.StatusError
				reply.Errors = append(reply.Errors, c.wrapErr("can't parse new successor for: "+c.id.String()))
				return
			}
			c.successor = &newSuccessor
			err = c.notify()
			if err != nil {
				reply.Purpose = chordpb.StatusError
				reply.Errors = append(reply.Errors, err.Error())
			}
		}
		return reply
	}
	next, err := c.checkNode(finger)
	if err != nil {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr("can't find remote node by id: "+finger.Id.String()))
		return reply
	}
	reply = next.Leave(ctx, req)
	return
}

func (c *chordImpl) Lookup(ctx *xrpc.XContext, req *chordpb.Message) (reply *chordpb.Message) {
	id := req.ID
	finger, ok := c.findFinger(id)

	reply = c.NewMessage(chordpb.StatusOk, req.Sender.Id, nil, nil)
	reply.ID = req.Sender.Id
	reply.Target = req.Sender
	if !ok {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr(fmt.Sprintf("can't find finger for %s at node: %s", id.String(), c.id.String())))
		return
	}
	if finger.Id == c.id {
		var err error
		if req.Purpose == chordpb.PredReq {
			p := c.predecessor
			if p == nil {
				p = c.self
			}
			data, err := json.Marshal(p)
			if err == nil {
				req.Body = data
			}
		}
		if err != nil {
			reply.Purpose = chordpb.StatusError
			reply.Errors = append(reply.Errors, c.wrapErr(err.Error()))
		}
		return reply
	}
	next, err := c.checkNode(finger)
	if err != nil {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr("can't find remote node by id: "+finger.Id.String()))
		return reply
	}
	reply = next.Lookup(ctx, req)
	return
}

func (c *chordImpl) FindSuccessor(ctx *xrpc.XContext, req *chordpb.Message) (reply *chordpb.Message) {
	id := req.ID
	finger, ok := c.findFinger(id)

	reply = c.NewMessage(chordpb.StatusOk, req.Sender.Id, nil, nil)
	reply.Target = req.Sender
	if !ok {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr(fmt.Sprintf("can't find finger for %s at node: %s", id.String(), c.id.String())))
		return
	}
	if !ok {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr(fmt.Sprintf("can't find finger for %s at node: %s", id.String(), c.id.String())))
		return
	}
	if finger.Id == c.id {
		sender := req.Sender
		if c.successor == nil {
			c.successor = &sender
			err := c.notify()
			if err != nil {
				reply.Purpose = chordpb.StatusError
				reply.Errors = append(reply.Errors, err.Error())
			}
		}
		return
	}
	next, err := c.checkNode(finger)
	if err != nil {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr("can't find remote node by id: "+finger.Id.String()))
		return reply
	}
	reply = next.FindSuccessor(ctx, req)
	return
}

func (c *chordImpl) Notify(ctx *xrpc.XContext, req *chordpb.Message) (reply *chordpb.Message) {
	id := req.ID
	finger, ok := c.findFinger(id)

	reply = c.NewMessage(chordpb.StatusOk, req.Sender.Id, nil, nil)
	reply.ID = req.Sender.Id
	reply.Target = req.Sender
	if !ok {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr(fmt.Sprintf("can't find finger for %s at node: %s", id.String(), c.id.String())))
		return
	}
	if finger.Id == c.id {
		sender := req.Sender
		if c.predecessor == nil || (c.predecessor.Id.Less(sender.Id) && sender.Id.Less(c.id)) {
			c.predecessor = &sender
			log.Debug("set predecessor to: ", sender.String())
		}
		return
	}
	next, err := c.checkNode(finger)
	if err != nil {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr("can't find remote node by id: "+finger.Id.String()))
		return
	}
	reply = next.Notify(ctx, req)
	return
}

func (c *chordImpl) HeartBeat(ctx *xrpc.XContext, req *chordpb.Message) (reply *chordpb.Message) {
	id := req.ID
	finger, ok := c.findFinger(id)

	reply = c.NewMessage(chordpb.StatusOk, req.Sender.Id, nil, nil)
	reply.ID = req.Sender.Id
	reply.Target = req.Sender
	if !ok {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr(fmt.Sprintf("can't find finger for %s at node: %s", id.String(), c.id.String())))
		return
	}
	if finger.Id == c.id {
		return reply
	}
	next, err := c.checkNode(finger)
	if err != nil {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr("can't find remote node by id: "+finger.Id.String()))
		return reply
	}
	reply = next.HeartBeat(ctx, req)
	return
}

func (c *chordImpl) Set(ctx *xrpc.XContext, req *chordpb.Message) (reply *chordpb.Message) {
	id := req.ID
	finger, ok := c.findFinger(id)

	reply = c.NewMessage(chordpb.StatusOk, req.Sender.Id, nil, nil)
	reply.ID = req.Sender.Id
	reply.Target = req.Sender
	if !ok {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr(fmt.Sprintf("can't find finger for %s at node: %s", id.String(), c.id.String())))
		return
	}
	if finger.Id == c.id {
		var err error
		fmt.Printf("set %s:%s\n", string(req.Key), string(req.Body))
		err = c.store.Set(req.Key, req.Body)
		if err != nil {
			reply.Purpose = chordpb.StatusError
			reply.Errors = append(reply.Errors, c.wrapErr(err.Error()))
		}
		return reply
	}
	next, err := c.checkNode(finger)
	if err != nil {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr("can't find remote node by id: "+finger.Id.String()))
		return reply
	}
	reply = next.Set(ctx, req)
	return
}

func (c *chordImpl) Get(ctx *xrpc.XContext, req *chordpb.Message) (reply *chordpb.Message) {
	id := req.ID
	finger, ok := c.findFinger(id)

	reply = c.NewMessage(chordpb.StatusOk, req.Sender.Id, nil, nil)
	reply.ID = req.Sender.Id
	reply.Target = req.Sender
	if !ok {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr(fmt.Sprintf("can't find finger for %s at node: %s", id.String(), c.id.String())))
		return
	}
	if finger.Id == c.id {
		var err error
		reply.Body, err = c.store.Get(req.Key)
		println("req key:", string(req.Key))
		if err != nil {
			reply.Purpose = chordpb.StatusError
			reply.Errors = append(reply.Errors, c.wrapErr(err.Error()))
		}
		println("get value:", string(reply.Body))

		return reply
	}
	next, err := c.checkNode(finger)
	if err != nil {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr("can't find remote node by id: "+finger.Id.String()))
		return reply
	}
	reply = next.Get(ctx, req)
	return
}

func (c *chordImpl) Del(ctx *xrpc.XContext, req *chordpb.Message) (reply *chordpb.Message) {
	id := req.ID
	finger, ok := c.findFinger(id)

	reply = c.NewMessage(chordpb.StatusOk, req.Sender.Id, nil, nil)
	reply.ID = req.Sender.Id
	reply.Target = req.Sender
	if !ok {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr(fmt.Sprintf("can't find finger for %s at node: %s", id.String(), c.id.String())))
		return
	}
	if finger.Id == c.id {
		var err error
		err = c.store.Del(req.Key)
		println("del key:", string(req.Key))
		if err != nil {
			reply.Purpose = chordpb.StatusError
			reply.Errors = append(reply.Errors, c.wrapErr(err.Error()))
		}
		return reply
	}
	next, err := c.checkNode(finger)
	if err != nil {
		reply.Purpose = chordpb.StatusError
		reply.Errors = append(reply.Errors, c.wrapErr("can't find remote node by id: "+finger.Id.String()))
		return reply
	}
	reply = next.Del(ctx, req)
	return
}

func (c *chordImpl) JoinNode(addr string) error {
	host, port := parseAddr(addr)
	id := c.h.Hash([]byte(addr))
	node := &chordpb.Node{
		Id:   id,
		Host: host,
		Port: port,
	}
	next, err := c.checkNode(node)
	if err != nil {
		return err
	}
	ctx := c.newXCtx()
	reply := next.Join(ctx, c.NewMessage(chordpb.NodeJoin, c.id, nil, nil))
	if reply == nil {
		return errors.New("join node failed")
	}
	if reply.Purpose == chordpb.StatusError {
		err = errors.New(strings.Join(reply.Errors, " || "))
		return err
	}
	successor := reply.Sender
	c.successor = &successor
	err = c.notify()
	for _, f := range c.fingerTable {
		reply = next.Lookup(ctx, c.NewMessage(chordpb.NodeAnn, f.id, nil, nil))
		if reply.Purpose == chordpb.StatusError {
			continue
		}
		f.node = reply.Sender
	}
	return err
}

func (c *chordImpl) newXCtx() *xrpc.XContext {
	xctx := xrpc.XBackground()
	return xctx
}

func (c *chordImpl) wrapErr(err string) string {
	return fmt.Sprintf("node %s: %s", c.id.String(), err)
}

func (c *chordImpl) sendMessage(ctx *xrpc.XContext, finger *chordpb.Node, req *chordpb.Message) (reply *chordpb.Message, err error) {
	next, err := c.checkNode(finger)
	if err != nil {
		return
	}

	switch req.Purpose {
	case chordpb.NodeJoin:
		reply = next.Set(ctx, req)
	case chordpb.NodeLeave:
		reply = next.Get(ctx, req)
	case chordpb.NodeNotify:
		reply = next.Del(ctx, req)
	case chordpb.NodeAnn:
		reply = next.Del(ctx, req)
	case chordpb.KeySet:
		reply = next.Set(ctx, req)
	case chordpb.KeyGet:
		reply = next.Get(ctx, req)
	case chordpb.KeyDel:
		reply = next.Del(ctx, req)
	case chordpb.SuccReq:
		reply = next.Set(ctx, req)
	case chordpb.PredReq:
		reply = next.Get(ctx, req)
	case chordpb.HeartBeat:
		reply = next.Del(ctx, req)
	default:
		err = errors.New("unknown chord purpose")
	}
	return reply, err
}

func (c *chordImpl) notify() error {
	println("set successor to: ", c.successor.String())
	next, err := c.checkNode(c.successor)
	if err != nil {
		return err
	}
	ctx := c.newXCtx()
	notify := c.NewMessage(chordpb.NodeNotify, c.successor.Id, nil, nil)
	notify.Target = *(c.successor)
	next.Notify(ctx, notify)
	return nil
}

func (c *chordImpl) findFinger(id chordpb.NodeID) (*chordpb.Node, bool) {
	nextNode := chordpb.Node{}
	if id.LE(c.id) && (c.predecessor == nil || c.predecessor.Id.Less(id)) {
		nextNode = *(c.self)
		return &nextNode, true
	}
	if c.successor == nil {
		nextNode = *(c.self)
		return &nextNode, true
	}
	if id.LE(c.successor.Id) {
		nextNode = *(c.successor)
		return &nextNode, true
	}
	n := len(c.fingerTable)
	for index, finger := range c.fingerTable {
		if finger.id.Equal(id) {
			nextNode = finger.node
			return &nextNode, true
		}
		next := c.fingerTable[(index+1)%n]
		if finger.id.Less(id) && id.LE(next.id) {
			nextNode = next.node
			return &nextNode, true
		}
		if finger.id.Less(id) && id.GE(next.id) {
			nextNode = next.node
			return &nextNode, true
		}
		if id.Less(finger.id) && id.LE(next.id) {
			nextNode = next.node
			return &nextNode, true
		}
	}
	return nil, false
}

func (c *chordImpl) NodeID() chordpb.NodeID {
	return c.id
}

func (c *chordImpl) NewMessage(purpose int, id chordpb.NodeID, key []byte, body []byte) *chordpb.Message {
	if len(id) == 0 {
		id = c.h.Hash(key)
	}
	return &chordpb.Message{
		ID:      id,
		Key:     key,
		Purpose: purpose,
		Sender:  *c.self,
		Hops:    0,
		Body:    body,
	}
}

func setupClient(cc *xrpc.ClientConn) {
	cryptoPlugin := cryptop.New()
	cryptoPlugin.SetKey(sessionID, sessionKey)
	cc.ApplyPlugins(cryptoPlugin)

	cc.SetHeaderArg("user", user)
	cc.SetHeaderArg("pass", pass)
	cc.SetHeaderArg(cryptop.Key, sessionID)
}

func (c *chordImpl) checkNode(node *chordpb.Node) (cc chordpb.ChordClient, err error) {
	var ok bool
	cc, ok = c.remoteNodes[node.Id]
	ctx := c.newXCtx()
	if ok {
		reply := cc.HeartBeat(ctx, c.NewMessage(chordpb.HeartBeat, c.id, nil, nil))
		if reply != nil && reply.Purpose == chordpb.StatusOk {
			return
		}
	}
	conn, err := xrpc.Dial("tcp", fmt.Sprintf("%s:%d", node.Host, node.Port), xrpc.WithInsecure(), xrpc.WithJsonCodec())
	if err != nil {
		return
	}
	c.amu.Lock()
	defer c.amu.Unlock()

	//setupClient(conn)
	cc = chordpb.NewChordClient(conn)
	c.remoteNodes[node.Id] = cc
	c.activeRecords[node.Id] = time.Now()
	c.conns[node.Id] = conn
	return
}

func (c *chordImpl) fixFingerTable() {
	c.amu.Lock()
	defer c.amu.Unlock()
	ctx := c.newXCtx()
	for index, finger := range c.fingerTable {
		req := c.NewMessage(chordpb.NodeAnn, finger.id, nil, nil)
		reply := c.FindSuccessor(ctx, req)
		if reply.Purpose == chordpb.StatusOk {
			newFinger := chordpb.Node{}
			err := json.Unmarshal(reply.Body, &newFinger)
			if err != nil {
				continue
			}
			c.fingerTable[index].node = newFinger
		}
	}
}

func (c *chordImpl) updateSuccessor() {
	ctx := c.newXCtx()
	if c.successor == nil {
		return
	}

	next, err := c.checkNode(c.successor)
	if err != nil {
		c.successor = nil
		return
	}
	reply := next.Lookup(ctx, c.NewMessage(chordpb.PredReq, c.successor.Id, nil, nil))
	if reply.Body == nil {
		return
	}
	newSuccessor := chordpb.Node{}
	err = json.Unmarshal(reply.Body, &newSuccessor)
	if err != nil {
		return
	}
	if c.id.Less(newSuccessor.Id) && newSuccessor.Id.Less(c.successor.Id) {
		c.successor = &newSuccessor
		c.notify()
	}
}

func (c *chordImpl) checkConns(exp time.Time) {
	timeout := time.Minute * 3
	c.amu.Lock()
	defer c.amu.Unlock()
	for id, at := range c.activeRecords {
		if exp.Sub(at) > timeout {
			c.conns[id].Close()
			delete(c.conns, id)
			delete(c.remoteNodes, id)
		}
		host, port := parseAddr(c.conns[id].Addr())
		c.checkNode(&chordpb.Node{
			Id:   id,
			Host: host,
			Port: port,
		})
	}
}

func (c *chordImpl) stabilize() {
	ticker := time.NewTicker(time.Second * 2)
	n := 0
	for {
		select {
		case exp := <-ticker.C:
			n++
			//log.Debugf("start stabilize[%d] at: %s", n, exp.String())
			c.checkConns(exp)
			c.updateSuccessor()
			c.fixFingerTable()
			//log.Debugf("finish stabilize[%d] at: %s", n, time.Now().String())
		case <-c.quit:
			log.Debug("quit stabilize")
			break
		}
	}
}

func NewBlake2bHasher() chordpb.Hasher {
	return &hasher{b: crypto.NewBlake2b()}
}

type hasher struct {
	b *crypto.Blake2b
}

func (h *hasher) Hash(data []byte) [128]byte {
	var hh [128]byte
	bh := h.b.HashBytes(data)
	copy(hh[:], bh)
	return hh
}

func (h *hasher) Size() int {
	return h.b.Size()
}

func NewSimpleKVStore() chordpb.KVStore {
	return &simpleKV{kvs: map[string][]byte{}}
}

type simpleKV struct {
	kvs map[string][]byte
}

func (m *simpleKV) Set(key, value []byte) (err error) {
	m.kvs[string(key)] = value
	return
}

func (m *simpleKV) Get(key []byte) (value []byte, err error) {
	value = m.kvs[string(key)]
	return
}

func (m *simpleKV) Del(key []byte) (err error) {
	delete(m.kvs, string(key))
	return
}
