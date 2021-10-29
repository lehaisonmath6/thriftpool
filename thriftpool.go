package thriftpool

import (
	"container/list"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/apache/thrift/lib/go/thrift"
)

const (
	CHECKINTERVAL = 60
)

type ThriftDial func(ip, port string, connTimeout time.Duration) (*IdleClient, error)
type ThriftClientClose func(c *IdleClient) error

type ThriftPool struct {
	Dial  ThriftDial
	Close ThriftClientClose

	lock        *sync.Mutex
	idle        list.List
	idleTimeout time.Duration
	connTimeout time.Duration
	maxConn     uint32
	count       uint32
	ip          string
	port        string
	closed      bool
}

type IdleClient struct {
	Socket *thrift.TSocket
	Client interface{}
}

func (c *IdleClient) SetConnTimeout(connTimeout uint32) {
	c.Socket.SetTimeout(time.Duration(connTimeout) * time.Second)
}

func (c *IdleClient) LocalAddr() net.Addr {
	return c.Socket.Conn().LocalAddr()
}

func (c *IdleClient) RemoteAddr() net.Addr {
	return c.Socket.Conn().RemoteAddr()
}

func (c *IdleClient) Check() bool {
	if c.Socket == nil || c.Client == nil {
		return false
	}
	return c.Socket.IsOpen()
}

type idleConn struct {
	c *IdleClient
	t time.Time
}

var nowFunc = time.Now

var (
	ErrOverMax          = errors.New("ErrOverMax")
	ErrInvalidConn      = errors.New("ErrInvalidConn")
	ErrPoolClosed       = errors.New("ErrPoolClosed")
	ErrSocketDisconnect = errors.New("ErrSocketDisconnect")
)

func NewThriftPool(ip, port string,
	maxConn, connTimeout, idleTimeout uint32,
	dial ThriftDial, closeFunc ThriftClientClose) *ThriftPool {

	thriftPool := &ThriftPool{
		Dial:        dial,
		Close:       closeFunc,
		ip:          ip,
		port:        port,
		lock:        new(sync.Mutex),
		maxConn:     maxConn,
		idleTimeout: time.Duration(idleTimeout) * time.Second,
		connTimeout: time.Duration(connTimeout) * time.Second,
		closed:      false,
		count:       0,
	}

	go thriftPool.ClearConn()

	return thriftPool
}

func (p *ThriftPool) Get() (*IdleClient, error) {
	p.lock.Lock()
	if p.closed {
		p.lock.Unlock()
		return nil, ErrPoolClosed
	}

	if p.idle.Len() == 0 && p.count >= p.maxConn {
		p.lock.Unlock()
		return nil, ErrOverMax
	}

	if p.idle.Len() == 0 {
		dial := p.Dial
		p.count += 1
		p.lock.Unlock()
		client, err := dial(p.ip, p.port, p.connTimeout)
		if err != nil {
			p.lock.Lock()
			if p.count > 0 {
				p.count -= 1
			}
			p.lock.Unlock()
			return nil, err
		}
		if !client.Check() {
			p.lock.Lock()
			if p.count > 0 {
				p.count -= 1
			}
			p.lock.Unlock()
			return nil, ErrSocketDisconnect
		}
		return client, nil
	} else {
		ele := p.idle.Front()
		idlec := ele.Value.(*idleConn)
		p.idle.Remove(ele)
		p.lock.Unlock()

		if !idlec.c.Check() {
			p.lock.Lock()
			if p.count > 0 {
				p.count -= 1
			}
			p.lock.Unlock()
			return nil, ErrSocketDisconnect
		}
		return idlec.c, nil
	}
}

func (p *ThriftPool) Put(client *IdleClient) error {
	if client == nil {
		return ErrInvalidConn
	}

	p.lock.Lock()
	if p.closed {
		p.lock.Unlock()

		err := p.Close(client)
		client = nil
		return err
	}

	if p.count > p.maxConn {
		if p.count > 0 {
			p.count -= 1
		}
		p.lock.Unlock()

		err := p.Close(client)
		client = nil
		return err
	}

	if !client.Check() {
		if p.count > 0 {
			p.count -= 1
		}
		p.lock.Unlock()

		err := p.Close(client)
		client = nil
		return err
	}

	p.idle.PushBack(&idleConn{
		c: client,
		t: nowFunc(),
	})
	p.lock.Unlock()

	return nil
}

func (p *ThriftPool) CloseErrConn(client *IdleClient) {
	if client == nil {
		return
	}

	p.lock.Lock()
	if p.count > 0 {
		p.count -= 1
	}
	p.lock.Unlock()

	p.Close(client)
	client = nil
	return
}

func (p *ThriftPool) CheckTimeout() {
	p.lock.Lock()
	for p.idle.Len() != 0 {
		ele := p.idle.Back()
		if ele == nil {
			break
		}
		v := ele.Value.(*idleConn)
		if v.t.Add(p.idleTimeout).After(nowFunc()) {
			break
		}

		//timeout && clear
		p.idle.Remove(ele)
		p.lock.Unlock()
		p.Close(v.c) //close client connection
		p.lock.Lock()
		if p.count > 0 {
			p.count -= 1
		}
	}
	p.lock.Unlock()

	return
}

func (p *ThriftPool) GetIdleCount() uint32 {
	return uint32(p.idle.Len())
}

func (p *ThriftPool) GetConnCount() uint32 {
	return p.count
}

func (p *ThriftPool) ClearConn() {
	for {
		p.CheckTimeout()
		time.Sleep(CHECKINTERVAL * time.Second)
	}
}

func (p *ThriftPool) Release() {
	p.lock.Lock()
	idle := p.idle
	p.idle.Init()
	p.closed = true
	p.count = 0
	p.lock.Unlock()

	for iter := idle.Front(); iter != nil; iter = iter.Next() {
		p.Close(iter.Value.(*idleConn).c)
	}
}

func (p *ThriftPool) Recover() {
	p.lock.Lock()
	if p.closed == true {
		p.closed = false
	}
	p.lock.Unlock()
}
