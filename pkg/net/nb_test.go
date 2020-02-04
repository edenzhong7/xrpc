package net_test

//import (
//	"fmt"
//	"testing"
//	"time"
//
//	"github.com/lesismal/nbio"
//)
//
//func onOpen(c *nbio.Conn) {
//	c.SetReadDeadline(time.Now().Add(time.Second * 10))
//	fmt.Println("onOpen:", c.LocalAddr().String(), c.RemoteAddr().String(), time.Now().Format("15:04:05.000"))
//}
//
//func onClose(c *nbio.Conn, err error) {
//	fmt.Println("onClose:", c.LocalAddr().String(), c.RemoteAddr().String(), time.Now().Format("15:04:05.000"), err)
//}
//
//func onData(c *nbio.Conn, data []byte) {
//	c.SetReadDeadline(time.Now().Add(time.Second * 10))
//	c.SetWriteDeadline(time.Now().Add(time.Second * 3))
//	c.Write(append([]byte{}, data...))
//}
//
//func TestNB(t *testing.T) {
//	g, err := nbio.NewGopher(nbio.Config{
//		Network:      "tcp",
//		Address:      ":8888",
//		NPoller:      2,
//		NWorker:      4,
//		QueueSize:    1024,
//		BufferSize:   1024 * 8,
//		BufferNum:    1024 * 2,
//		PollInterval: time.Millisecond * 200,
//		MaxTimeout:   time.Second * 10,
//	})
//	if err != nil {
//		fmt.Printf("nbio.New failed: %v\n", err)
//		return
//	}
//
//	g.OnOpen(onOpen)
//	g.OnClose(onClose)
//	g.OnData(onData)
//
//	err = g.Start()
//	if err != nil {
//		fmt.Printf("nbio.Start failed: %v\n", err)
//		return
//	}
//
//	go func() {
//		for {
//			time.Sleep(time.Second * 5)
//			fmt.Println(g.State().String())
//		}
//	}()
//	g.Wait()
//}
