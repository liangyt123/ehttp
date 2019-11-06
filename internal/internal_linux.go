// Copyright 2017 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package internal

import (
	//"fmt"
	"fmt"
	"syscall"
) //

//// Poll ...
type Poll struct {
	fd    int // // epoll fd
	wfd   int // // wake fd
	notes noteQueue
}

// OpenPoll ...
func OpenPoll() *Poll {
	l := new(Poll)
	p, err := syscall.EpollCreate1(0)
	if err != nil {
		panic(err)
	}
	l.fd = p
	r0, _, e0 := syscall.Syscall(syscall.SYS_EVENTFD2, 0, 0, 0)
	if e0 != 0 {
		syscall.Close(p)
		panic(err)
	}
	l.wfd = int(r0)
	l.AddRead(l.wfd)
	return l
}

// Close ...
func (p *Poll) Close() error {
	if err := syscall.Close(p.wfd); err != nil {
		return err
	}
	return syscall.Close(p.fd)
}

// Trigger ...
func (p *Poll) Trigger(note interface{}) error {
	p.notes.Add(note)
	_, err := syscall.Write(p.wfd, []byte{0, 0, 0, 0, 0, 0, 0, 1})
	return err
}

func (p *Poll) FD() int {
	return p.fd
}

func (p *Poll) NoteQueue() noteQueue {
	return p.notes
}

func (p *Poll) WFD() int {
	return p.wfd
}

// Wait ...
func (p *Poll) Wait(iter func(fd int, note interface{}) error) error {
	//fmt.Println("i am waitting haha,", p.fd)
	events := make([]syscall.EpollEvent, 64)
	for {
		n, err := syscall.EpollWait(p.fd, events, -1)
		if err != nil && err != syscall.EINTR {
			//fmt.Println("ERROR syscall.EpollWait:", n)
			return err
		}
		//fmt.Println("syscall.EpollWait:", n, p.fd)
		if p.notes.Len() != 0 {
			if err := p.notes.ForEach(func(note interface{}) error {
				fmt.Println("len(notes)", p.notes.Len())
				return iter(0, note)
			}); err != nil {
				return err
			}
		}

		for i := 0; i < n; i++ {
			if fd := int(events[i].Fd); fd != p.wfd {
				if err := iter(fd, nil); err != nil {
					return err
				}
			} else {

			}
		}
	}
}

// AddReadWrite ...
func (p *Poll) AddReadWrite(fd int) {
	//fmt.Println("AddReadWrite:", fd)
	if err := syscall.EpollCtl(p.fd, syscall.EPOLL_CTL_ADD, fd,
		&syscall.EpollEvent{Fd: int32(fd),
			Events: syscall.EPOLLIN | syscall.EPOLLOUT,
		},
	); err != nil {
		panic(err)
	}
}

// AddRead ...
func (p *Poll) AddRead(fd int) {
	//fmt.Println("AddRead:", fd)
	if err := syscall.EpollCtl(p.fd, syscall.EPOLL_CTL_ADD, fd,
		&syscall.EpollEvent{Fd: int32(fd),
			Events: syscall.EPOLLIN,
		},
	); err != nil {
		panic(err)
	}
}

// ModRead ...
func (p *Poll) ModRead(fd int) {
	//fmt.Println("ModRead:", fd)
	if err := syscall.EpollCtl(p.fd, syscall.EPOLL_CTL_MOD, fd,
		&syscall.EpollEvent{Fd: int32(fd),
			Events: syscall.EPOLLIN,
		},
	); err != nil {
		panic(err)
	}
}

// ModReadWrite ...
func (p *Poll) ModReadWrite(fd int) {
	//fmt.Println("ModReadWrite:", fd)
	if err := syscall.EpollCtl(p.fd, syscall.EPOLL_CTL_MOD, fd,
		&syscall.EpollEvent{Fd: int32(fd),
			Events: syscall.EPOLLIN | syscall.EPOLLOUT,
		},
	); err != nil {
		panic(err)
	}
}

// ModDetach ...
func (p *Poll) ModDetach(fd int) {
	if err := syscall.EpollCtl(p.fd, syscall.EPOLL_CTL_DEL, fd,
		&syscall.EpollEvent{Fd: int32(fd),
			Events: syscall.EPOLLIN | syscall.EPOLLOUT,
		},
	); err != nil {
		panic(err)
	}
}
