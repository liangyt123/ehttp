// Copyright 2019 Yuanting Liang. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build darwin netbsd freebsd openbsd dragonfly linux

package evio

import (
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"ehttp/internal"
)

type conn struct {
	fd         int              // file descriptor
	lnidx      int              // listener index in the server lns list
	out        []byte           // write buffer
	sa         syscall.Sockaddr // remote socket address
	packet     []byte           // read buffer
	reuse      bool             // should reuse input buffer
	opened     bool             // connection opened event fired
	action     Action           // next user action
	ctx        interface{}      // user-defined context
	addrIndex  int              // index of listening address
	localAddr  net.Addr         // local addre
	remoteAddr net.Addr         // remote addr
	loop       *loop            // connected loop
}

func (c *conn) Context() interface{}       { return c.ctx }
func (c *conn) SetContext(ctx interface{}) { c.ctx = ctx }
func (c *conn) AddrIndex() int             { return c.addrIndex }
func (c *conn) LocalAddr() net.Addr        { return c.localAddr }
func (c *conn) RemoteAddr() net.Addr       { return c.remoteAddr }
func (c *conn) Wake() {
	if c.loop != nil {
		c.loop.poll.Trigger(c)
	}
}

type loop struct {
	idx     int            // loop index in the server loops list
	poll    *internal.Poll // epoll or kqueue
	packet  []byte         // read packet buffer
	fdconns map[int]*conn  // loop connections fd -> conn
	count   int32          // connection count
	ctx     context
}

func (l *loop) CloseConn(c *conn, err error) error {
	atomic.AddInt32(&l.count, -1)
	delete(l.fdconns, c.fd)
	syscall.Close(c.fd)
	if l.ctx.events.Closed != nil {
		switch l.ctx.events.Closed(c, err) {
		case None:
		case Shutdown:
			return errClosing
		}
	}
	return nil
}

func (l *loop) DetachConn(c *conn, err error) error {
	if l.ctx.events.Detached == nil {
		return l.CloseConn(c, err)
	}
	l.poll.ModDetach(c.fd)

	atomic.AddInt32(&l.count, -1)
	delete(l.fdconns, c.fd)
	if err := syscall.SetNonblock(c.fd, false); err != nil {
		return err
	}
	switch l.ctx.events.Detached(c, &detachedConn{fd: c.fd}) {
	case None:
	case Shutdown:
		return errClosing
	}
	return nil
}

//make the

// func (l *loop)Note(note interface{}) error {
// 	var err error
// 	switch v := note.(type) {
// 	case time.Duration:
// 		delay, action := l.ctx.events.Tick()
// 		switch action {
// 		case None:
// 		case Shutdown:
// 			err = errClosing
// 		}
// 		l.tch <- delay
// 	case error: // shutdown
// 		err = v
// 	case *conn:
// 		// Wake called for connection
// 		if l.fdconns[v.fd] != v {
// 			return nil // ignore stale wakes
// 		}
// 		return l.loopWake(v)
// 	}
// 	return err
// }

func (l *loop) UDPRead(lnaddr net.Addr, fd int) error {
	n, sa, err := syscall.Recvfrom(fd, l.packet, 0)
	if err != nil || n == 0 {
		return nil
	}
	if l.ctx.events.Data != nil {
		var sa6 syscall.SockaddrInet6
		switch sa := sa.(type) {
		case *syscall.SockaddrInet4:
			sa6.ZoneId = 0
			sa6.Port = sa.Port
			for i := 0; i < 12; i++ {
				sa6.Addr[i] = 0
			}
			sa6.Addr[12] = sa.Addr[0]
			sa6.Addr[13] = sa.Addr[1]
			sa6.Addr[14] = sa.Addr[2]
			sa6.Addr[15] = sa.Addr[3]
		case *syscall.SockaddrInet6:
			sa6 = *sa
		}
		c := &conn{}
		c.addrIndex = 1
		c.localAddr = lnaddr
		c.remoteAddr = internal.SockaddrToAddr(&sa6)
		in := append([]byte{}, l.packet[:n]...)
		out, action := l.ctx.events.Data(c, in)
		if len(out) > 0 {
			if l.ctx.events.PreWrite != nil {
				l.ctx.events.PreWrite()
			}
			syscall.Sendto(fd, out, 0, sa)
		}
		switch action {
		case Shutdown:
			return errClosing
		}
	}
	return nil
}

func (l *loop) Opened(c *conn) error {
	c.opened = true
	c.remoteAddr = internal.SockaddrToAddr(c.sa)
	if l.ctx.events.Opened != nil {
		out, opts, action := l.ctx.events.Opened(c)
		if len(out) > 0 {
			c.out = append([]byte{}, out...)
		}
		c.action = action
		c.reuse = opts.ReuseInputBuffer
		if opts.TCPKeepAlive > 0 {
			internal.SetKeepAlive(c.fd, int(opts.TCPKeepAlive/time.Second))
		}
	}
	if len(c.out) == 0 && c.action == None {
		l.poll.ModRead(c.fd)
	}
	return nil
}

func (l *loop) Write(c *conn) error {
	if l.ctx.events.PreWrite != nil {
		l.ctx.events.PreWrite()
	}
	n, err := syscall.Write(c.fd, c.out)
	if err != nil {
		if err == syscall.EAGAIN {
			return nil
		}
		return l.CloseConn(c, err)
	}
	if n == len(c.out) {
		c.out = nil
	} else {
		c.out = c.out[n:]
	}
	if len(c.out) == 0 && c.action == None {
		l.poll.ModRead(c.fd)
	}
	return nil
}

func (l *loop) Action(c *conn) error {
	switch c.action {
	default:
		c.action = None
	case Close:
		return l.CloseConn(c, nil)
	case Shutdown:
		return errClosing
	case Detach:
		return l.DetachConn(c, nil)
	}
	if len(c.out) == 0 && c.action == None {
		l.poll.ModRead(c.fd)
	}
	return nil
}

func (l *loop) Wake(c *conn) error {
	if l.ctx.events.Data == nil {
		return nil
	}
	out, action := l.ctx.events.Data(c, nil)
	c.action = action
	if len(out) > 0 {
		c.out = append([]byte{}, out...)
	}
	if len(c.out) != 0 || c.action != None {
		l.poll.ModReadWrite(c.fd)
	}
	return nil
}

func (l *loop) Read(c *conn) error {
	var in []byte
	n, err := syscall.Read(c.fd, c.packet)
	if n == 0 || err != nil {
		if err == syscall.EAGAIN {
			return nil
		}
		return l.CloseConn(c, err)
	}
	in = c.packet[:n]
	if !c.reuse {
		in = append([]byte{}, in...)
	}
	if l.ctx.events.Data != nil {
		out, action := l.ctx.events.Data(c, in)
		c.action = action
		if len(out) > 0 {
			c.out = append([]byte{}, out...)
		}
	}
	if len(c.out) != 0 || c.action != None {
		l.poll.ModReadWrite(c.fd)
	}
	return nil
}

func (l *loop) workRun() {
	defer func() {
		fmt.Println("--work run stopped --", l.idx)
		l.ctx.signalShutdown()
		l.ctx.wg.Done()
	}()

	//make a work run, only  a cpu
	if l.idx == 0 && l.ctx.events.Tick != nil {
		go l.Ticker()
	}

	fmt.Println("--work run started --", l.idx)
	//fmt.Println("i am waitting haha,", p.fd)
	events := make([]syscall.EpollEvent, 64)
	fd := l.poll.FD()
	for {

		n, err := syscall.EpollWait(fd, events, -1)
		if err != nil && err != syscall.EINTR {
			//fmt.Println("ERROR syscall.EpollWait:", n)
			continue
		}
		//the note loop event
		//fmt.Println("syscall.EpollWait:", n, p.fd)
		for i := 0; i < n; i++ {
			if fd := int(events[i].Fd); fd != l.poll.WFD() {
				c := l.fdconns[fd]
				switch {
				case !c.opened:
					l.Opened(c)
				case len(c.out) > 0:
					l.Write(c)
				case c.action != None:
					l.Action(c)
				default:
					l.Read(c)
				}
			}
		}
	}
}

func (l *loop) Ticker() {
	for {
		if err := l.poll.Trigger(time.Duration(0)); err != nil {
			break
		}
		time.Sleep(<-l.ctx.tch)
	}
}
