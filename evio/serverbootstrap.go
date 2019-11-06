// Copyright 2019 Yuanting Liang. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build darwin netbsd freebsd openbsd dragonfly linux
package evio

import (
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ehttp/internal"
)

type context struct {
	events   EventsFunc         // user EventsFunc
	wg       sync.WaitGroup     // loop close waitgroup
	cond     *sync.Cond         // shutdown signaler
	balance  LoadBalance        // load balancing method
	accepted uintptr            // accept counter
	tch      chan time.Duration // ticker channel\
	//ticktm   time.Time      // next tick time
}

// waitForShutdown waits for a signal to shutdown
func (s *context) waitForShutdown() {
	s.cond.L.Lock()
	s.cond.Wait()
	s.cond.L.Unlock()
}

// signalShutdown signals a shutdown an begins server closing
func (s *context) signalShutdown() {
	s.cond.L.Lock()
	s.cond.Signal()
	s.cond.L.Unlock()
}

//the context of a small server
type ServerBootstrap struct {
	NumProc    int
	Options    *Options
	NetAddress string
	Ln         listener
	//to loop all epoll function
	WorkloopGroups loopGroup
	MainLoop       *loop
	//system api
	ctx context
}

//each loopgroup can hold a loop should be red-black tree
type loopGroup struct {
	loops []*loop
}

// notify all loops to close by closing all listeners
func (lg *loopGroup) notifyErrClose() {
	for _, i := range lg.loops {
		i.poll.Trigger(errClosing)
	}
}

//  close loops and all outstanding connections
func (lg *loopGroup) closeOutstandingClose() {
	for _, i := range lg.loops {
		for _, c := range i.fdconns {
			i.CloseConn(c, nil)
		}
		i.poll.Close()
	}
}

func (s *ServerBootstrap) initListener() error {
	var ln listener
	ln.network, ln.addr, ln.opts, _ = parseAddr(s.NetAddress)

	if ln.network == "unix" {
		os.RemoveAll(ln.addr)
	}
	var err error
	if ln.network == "udp" {
		if ln.opts.reusePort {
			ln.pconn, err = reuseportListenPacket(ln.network, ln.addr)
		} else {
			ln.pconn, err = net.ListenPacket(ln.network, ln.addr)
		}
	} else {
		if ln.opts.reusePort {
			ln.ln, err = reuseportListen(ln.network, ln.addr)
		} else {
			ln.ln, err = net.Listen(ln.network, ln.addr)
		}
	}

	if err != nil {
		return err
	}
	if ln.pconn != nil {
		ln.lnaddr = ln.pconn.LocalAddr()
	} else {
		ln.lnaddr = ln.ln.Addr()
	}

	if err := ln.system(); err != nil {
		return err
	}
	s.Ln = ln
	return nil
}

func (s *ServerBootstrap) crash() {
	// wait on a signal for shutdown
	s.ctx.waitForShutdown()
	s.WorkloopGroups.notifyErrClose()

	if s.MainLoop != nil {
		s.MainLoop.poll.Trigger(errClosing)
	}
	// wait on all loops to complete reading events
	s.ctx.wg.Wait()
	// close loops and all outstanding connections
	s.WorkloopGroups.closeOutstandingClose()

	// stop main loop
	if s.MainLoop != nil {
		for _, c := range s.MainLoop.fdconns {
			s.MainLoop.CloseConn(c, nil)
		}
	}
}

//once the goruntine panic, it will not work all right
func (s *ServerBootstrap) startMainloop() error {
	defer func() {
		s.ctx.wg.Done()
		s.ctx.signalShutdown()
	}()

	pid := 0
	s.MainLoop = &loop{
		idx:     pid,
		poll:    internal.OpenPoll(),
		packet:  make([]byte, 0xFFFF),
		fdconns: make(map[int]*conn),
		ctx:     s.ctx,
	}

	//fmt.Printf("l is :%+v\n,ln.fd:%d", *s.MainLoop.poll, s.Ln.fd)
	//fmt.Println("--mainloop started -- ")
	s.MainLoop.poll.AddRead(s.Ln.fd)

	for {
		events := make([]syscall.EpollEvent, 64)
		n, err := syscall.EpollWait(s.MainLoop.poll.FD(), events, -1)
		if err != nil && err != syscall.EINTR {
			continue
		}
		for i := 0; i < n; i++ {
			if fd := int(events[i].Fd); fd != s.MainLoop.poll.WFD() {
				nfd, sa, err := syscall.Accept(fd)
				if err != nil {
					if err == syscall.EAGAIN {
						continue
					}
					continue
				}
				if err := syscall.SetNonblock(nfd, true); err != nil {
					continue
				}
				c := &conn{fd: nfd, sa: sa, lnidx: pid, loop: s.MainLoop, packet: make([]byte, 0xFFFF)}
				//find the loop
				idx := nfd % s.NumProc
				//fmt.Println("connect,nfd:", nfd, " idx:", idx)
				l := s.WorkloopGroups.loops[idx]
				l.fdconns[nfd] = c
				l.poll.AddReadWrite(c.fd)
				atomic.AddInt32(&l.count, 1)
			}
		}
	}
	return nil
}

//make the fd more effective  so do the route
//when the mainloop accept a connection,it will call the workloopgroup
//there are Numproc goroutine deal with each accept
//group has lots of loop (each loop means different fd() for a mod hash alog)
func (s *ServerBootstrap) startWorkloopgroup() error {

	//the number of cpu
	//fmt.Println("startWorkloopgroup")
	//new loopGroup

	for i := 1; i <= s.NumProc; i++ {
		loop := &loop{
			idx:     i,
			poll:    internal.OpenPoll(),
			packet:  make([]byte, 0xFFFF),
			fdconns: make(map[int]*conn),
			ctx:     s.ctx,
		}
		s.WorkloopGroups.loops = append(s.WorkloopGroups.loops, loop)
	}

	for _, l := range s.WorkloopGroups.loops {
		go l.workRun()
	}

	return nil
}

func (s *ServerBootstrap) serve() error {
	defer s.crash()
	//the main loop listen the socket
	//fmt.Println("serve")
	s.ctx.wg.Add(1)
	s.startWorkloopgroup()
	s.ctx.wg.Add(s.NumProc)
	s.startMainloop()
	s.ctx.wg.Wait()
	return nil
}

//TODO: more initoption
func ListenAndServe(netAddress string, e EventsFunc) {

	s := ServerBootstrap{NetAddress: netAddress,
		NumProc: runtime.NumCPU() - 1,
		ctx: context{
			events: e,
			cond:   sync.NewCond(&sync.Mutex{}),
			tch:    make(chan time.Duration),
		},
	}
	err := s.initListener()
	if err != nil {
		panic(err.Error())
	}
	if err = s.serve(); err != nil {
		panic("serve failed")
	}
}
