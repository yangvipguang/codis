// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package proxy

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/CodisLabs/codis/pkg/proxy/redis"
	"github.com/CodisLabs/codis/pkg/utils/errors"
	"github.com/CodisLabs/codis/pkg/utils/log"
	"github.com/CodisLabs/codis/pkg/utils/math2"
	"github.com/CodisLabs/codis/pkg/utils/sync2/atomic2"
)

type BackendConn struct {
	stop sync.Once
	addr string

	input chan *Request
	ready atomic2.Bool
	retry struct {
		fails int
		delay int
	}

	closed atomic2.Bool
	config *Config
}

func NewBackendConn(addr string, config *Config) *BackendConn {
	bc := &BackendConn{
		addr: addr, config: config,
	}
	bc.input = make(chan *Request, 1024)

	go bc.run()

	return bc
}

func (bc *BackendConn) Addr() string {
	return bc.addr
}

func (bc *BackendConn) Close() {
	bc.stop.Do(func() {
		close(bc.input)
	})
	bc.closed.Set(true)
}

func (bc *BackendConn) IsConnected() bool {
	return bc.ready.Get()
}

func (bc *BackendConn) PushBack(r *Request) {
	if r.Batch != nil {
		r.Batch.Add(1)
	}
	bc.input <- r
}

func (bc *BackendConn) KeepAlive() bool {
	if len(bc.input) != 0 {
		return false
	}
	m := &Request{}
	m.Multi = []*redis.Resp{
		redis.NewBulkBytes([]byte("PING")),
	}
	bc.PushBack(m)
	return true
}

func (bc *BackendConn) newBackendReader(round int, config *Config) (*redis.Conn, chan<- *Request, error) {
	c, err := redis.DialTimeout(bc.addr, time.Second*5,
		config.BackendRecvBufsize.Int(),
		config.BackendSendBufsize.Int())
	if err != nil {
		return nil, nil, err
	}
	c.ReaderTimeout = config.BackendRecvTimeout.Get()
	c.WriterTimeout = config.BackendSendTimeout.Get()
	c.SetKeepAlivePeriod(config.BackendKeepAlivePeriod.Get())

	if err := bc.verifyAuth(c, config.ProductAuth); err != nil {
		c.Close()
		return nil, nil, err
	}

	tasks := make(chan *Request, config.BackendMaxPipeline)
	go bc.loopReader(tasks, c, round)

	return c, tasks, nil
}

func (bc *BackendConn) verifyAuth(c *redis.Conn, auth string) error {
	if auth == "" {
		return nil
	}

	multi := []*redis.Resp{
		redis.NewBulkBytes([]byte("AUTH")),
		redis.NewBulkBytes([]byte(auth)),
	}

	if err := c.EncodeMultiBulk(multi, true); err != nil {
		return err
	}

	resp, err := c.Decode()
	switch {
	case err != nil:
		return err
	case resp == nil:
		return ErrRespIsRequired
	case resp.IsError():
		return fmt.Errorf("error resp: %s", resp.Value)
	case resp.IsString():
		return nil
	default:
		return fmt.Errorf("error resp: should be string, but got %s", resp.Type)
	}
}

func (bc *BackendConn) setResponse(r *Request, resp *redis.Resp, err error) error {
	r.Resp, r.Err = resp, err
	if r.Group != nil {
		r.Group.Done()
	}
	if r.Batch != nil {
		r.Batch.Done()
	}
	return err
}

var (
	ErrBackendConnReset = errors.New("backend conn reset")
	ErrRequestIsBroken  = errors.New("request is broken")
)

func (bc *BackendConn) run() {
	log.Warnf("backend conn [%p] to %s, start service", bc, bc.addr)
	for k := 0; !bc.closed.Get(); k++ {
		log.Warnf("backend conn [%p] to %s, rounds-[%d]", bc, bc.addr, k)
		if err := bc.loopWriter(k); err != nil {
			bc.delayBeforeRetry()
		}
	}
	log.Warnf("backend conn [%p] to %s, stop and exit", bc, bc.addr)
}

func (bc *BackendConn) loopReader(tasks <-chan *Request, c *redis.Conn, round int) (err error) {
	defer func() {
		c.Close()
		for r := range tasks {
			bc.setResponse(r, nil, ErrBackendConnReset)
		}
		log.WarnErrorf(err, "backend conn [%p] to %s, reader-[%d] exit", bc, bc.addr, round)
	}()
	for r := range tasks {
		resp, err := c.Decode()
		if err != nil {
			return bc.setResponse(r, nil, fmt.Errorf("backend conn failure, %s", err))
		}
		bc.setResponse(r, resp, nil)
	}
	return nil
}

func (bc *BackendConn) delayBeforeRetry() {
	bc.retry.fails += 1
	if bc.retry.fails <= 10 {
		return
	}
	bc.retry.delay = math2.MinMaxInt(bc.retry.delay*2, 50, 5000)
	timeout := time.After(time.Millisecond * time.Duration(bc.retry.delay))
	for !bc.closed.Get() {
		select {
		case <-timeout:
			return
		case r, ok := <-bc.input:
			if !ok {
				return
			}
			bc.setResponse(r, nil, ErrBackendConnReset)
		}
	}
}

func (bc *BackendConn) loopWriter(round int) (err error) {
	defer func() {
		for i := len(bc.input); i != 0; i-- {
			r := <-bc.input
			bc.setResponse(r, nil, ErrBackendConnReset)
		}
		log.WarnErrorf(err, "backend conn [%p] to %s, writer-[%d] exit", bc, bc.addr, round)
	}()
	c, tasks, err := bc.newBackendReader(round, bc.config)
	if err != nil {
		return err
	}
	defer close(tasks)

	defer bc.ready.Set(false)

	bc.ready.Set(true)
	bc.retry.fails = 0
	bc.retry.delay = 0

	p := c.FlushEncoder()
	p.MaxInterval = time.Millisecond
	p.MaxBuffered = math2.MinInt(256, cap(tasks))

	for r := range bc.input {
		if r.IsReadOnly() && r.IsBroken() {
			bc.setResponse(r, nil, ErrRequestIsBroken)
			continue
		}
		if err := p.EncodeMultiBulk(r.Multi); err != nil {
			return bc.setResponse(r, nil, fmt.Errorf("backend conn failure, %s", err))
		}
		if err := p.Flush(len(bc.input) == 0); err != nil {
			return bc.setResponse(r, nil, fmt.Errorf("backend conn failure, %s", err))
		} else {
			tasks <- r
		}
	}
	return nil
}

type sharedBackendConn struct {
	addr string
	host []byte
	port []byte

	owner *sharedBackendConnPool
	conns []*BackendConn

	refcnt int
}

func newSharedBackendConn(addr string, config *Config, pool *sharedBackendConnPool) *sharedBackendConn {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		log.ErrorErrorf(err, "split host-port failed, address = %s", addr)
	}
	s := &sharedBackendConn{
		addr: addr,
		host: []byte(host), port: []byte(port),
	}
	s.owner = pool
	s.conns = make([]*BackendConn, pool.parallel)
	for i := range s.conns {
		s.conns[i] = NewBackendConn(addr, config)
	}
	s.refcnt = 1
	return s
}

func (s *sharedBackendConn) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

func (s *sharedBackendConn) Release() {
	if s == nil {
		return
	}
	if s.refcnt <= 0 {
		log.Panicf("shared backend conn has been closed, close too many times")
	} else {
		s.refcnt--
	}
	if s.refcnt != 0 {
		return
	}
	for i := range s.conns {
		s.conns[i].Close()
	}
	delete(s.owner.pool, s.addr)
}

func (s *sharedBackendConn) Retain() *sharedBackendConn {
	if s == nil {
		return nil
	}
	if s.refcnt <= 0 {
		log.Panicf("shared backend conn has been closed")
	} else {
		s.refcnt++
	}
	return s
}

func (s *sharedBackendConn) KeepAlive() {
	if s == nil {
		return
	}
	for i := range s.conns {
		s.conns[i].KeepAlive()
	}
}

func (s *sharedBackendConn) BackendConn(seed uint, must bool) *BackendConn {
	if s == nil {
		return nil
	}
	var i = seed
	for _ = range s.conns {
		i = (i + 1) % uint(len(s.conns))
		if bc := s.conns[i]; bc.IsConnected() {
			return bc
		}
	}
	if !must {
		return nil
	}
	return s.conns[0]
}

type sharedBackendConnPool struct {
	parallel int

	pool map[string]*sharedBackendConn
}

func newSharedBackendConnPool(parallel int) *sharedBackendConnPool {
	p := &sharedBackendConnPool{}
	p.parallel = math2.MaxInt(1, parallel)
	p.pool = make(map[string]*sharedBackendConn)
	return p
}

func (p *sharedBackendConnPool) KeepAlive() {
	for _, bc := range p.pool {
		bc.KeepAlive()
	}
}

func (p *sharedBackendConnPool) Get(addr string) *sharedBackendConn {
	return p.pool[addr]
}

func (p *sharedBackendConnPool) Retain(addr string, config *Config) *sharedBackendConn {
	if bc := p.pool[addr]; bc != nil {
		return bc.Retain()
	} else {
		bc = newSharedBackendConn(addr, config, p)
		p.pool[addr] = bc
		return bc
	}
}
