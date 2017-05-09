package wsmux

import (
	"encoding/binary"
	// "io"
	"io/ioutil"
	// "log"
	"net"
	// "runtime"
	"bytes"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	// "github.com/taskcluster/slugid-go/slugid"
)

const (
	defaultQueueSize       = 10
	defaultStreamQueueSize = 10
)

type Session struct {
	mu       sync.Mutex
	streams  map[uint32]*stream
	streamCh chan *stream
	writes   chan frame
	conn     *websocket.Conn

	acceptDeadline time.Time
	readDeadline   time.Time
	writeDeadline  time.Time

	logger Logger

	closed       atomic.Value
	remoteClosed atomic.Value

	nextID uint32

	remoteCloseCallback func()
}

func newSession(conn *websocket.Conn, server bool, conf *Config) *Session {
	if conf == nil {
		conf = &Config{}
	}
	s := &Session{}
	s.conn = conn
	s.streams = make(map[uint32]*stream)
	s.streamCh = make(chan *stream, defaultStreamQueueSize)
	s.writes = make(chan frame, defaultQueueSize)

	s.acceptDeadline = conf.AcceptDeadline
	s.readDeadline = conf.ReadDeadline
	s.writeDeadline = conf.WriteDeadline
	s.remoteCloseCallback = conf.RemoteCloseCallback

	s.closed.Store(false)
	s.remoteClosed.Store(false)

	if server {
		s.nextID = 0
	} else {
		s.nextID = 1
	}

	if conf.Log == nil {
		s.logger = &nilLogger{}
	} else {
		s.logger = conf.Log
	}
	go s.sendLoop()
	go s.recvLoop()
	return s
}

func (s *Session) Accept() (net.Conn, error) {
	var timeout <-chan time.Time
	var timer *time.Timer
	if !s.acceptDeadline.IsZero() {
		timer = time.NewTimer(s.acceptDeadline.Sub(time.Now()))
		timeout = timer.C
	}

	select {
	case <-timeout:
		return nil, ErrAcceptTimeout
	case str := <-s.streamCh:
		return str, nil
	}
}

func (s *Session) Open() (net.Conn, error) {
	id := atomic.LoadUint32(&s.nextID)
	s.mu.Lock()
	if _, ok := s.streams[id]; ok {
		s.mu.Unlock()
		return nil, ErrDuplicateStream
	}
	s.mu.Unlock()
	s.writes <- newSynFrame(id)
	str := newStream(id, s)
	atomic.AddUint32(&s.nextID, 2)
	s.mu.Lock()
	s.streams[id] = str
	s.mu.Unlock()
	return str, nil
}

func (s *Session) Close() error {
	if s.closed.Load() == true {
		return ErrBrokenPipe
	}
	s.closed.Store(true)
	s.writes <- newClsFrame(0)
	return nil
}

func (s *Session) Addr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *Session) sendLoop() {
	for fr := range s.writes {
		err := s.conn.WriteMessage(websocket.BinaryMessage, fr.Write())
		s.logger.Printf("wrote message: %v", fr)
		if err != nil {
			s.logger.Print(err)
		}
	}
}

func (s *Session) recvLoop() {
	for {
		_, msgReader, err := s.conn.NextReader()
		if err != nil {
			_ = s.Close()
			break
		}
		h, err := getHeader(msgReader)
		id, msgType := h.id(), h.msg()
		switch msgType {
		//Used for creating a new stream
		case msgSYN:
			s.mu.Lock()
			if _, ok := s.streams[id]; ok {
				s.logger.Printf("received duplicate syn frame for stream %d", id)
				s.mu.Unlock()
				break
			}
			str := newStream(id, s)
			s.streams[id] = str
			s.asyncPushStream(str)
			s.mu.Unlock()

		//received data
		case msgDAT:
			s.mu.Lock()
			str, ok := s.streams[id]
			s.mu.Unlock()
			if !ok {
				s.logger.Printf("received data frame for unknown stream %d", id)
				break
			}
			b, err := ioutil.ReadAll(msgReader)
			if err != nil {
				s.logger.Print(err)
				break
			}
			str.addToBuffer(b)
			str.notifyRead()
			s.logger.Printf("received DAT frame on stream %d: %v", id, bytes.NewBuffer(b))

		//received ack frame
		case msgACK:
			s.mu.Lock()
			str, ok := s.streams[id]
			s.mu.Unlock()
			if !ok {
				s.logger.Printf("received ack frame for unknown stream %d", id)
				break
			}

			b := make([]byte, 4)
			_, err := msgReader.Read(b)
			if err != nil {
				s.logger.Print(err)
				break
			}
			read := binary.LittleEndian.Uint32(b)
			s.logger.Printf("received ack frame: id %d: remote read %d bytes", id, read)
			str.updateRemoteCapacity(read)
			str.notifyWrite()

		// received fin frame
		case msgFIN:
			s.mu.Lock()
			str, ok := s.streams[id]
			s.mu.Unlock()
			if !ok {
				s.logger.Printf("received fin frame for unknown stream %d", id)
				break
			}

			err := str.setRemoteClosed()
			if str != nil {
				s.logger.Print(err)
			}
			s.logger.Printf("remote stream %d closed connection", id)

		case msgCLS:
			s.logger.Printf("remote session closed")
			s.remoteClosed.Store(true)
			if s.remoteCloseCallback != nil {
				s.remoteCloseCallback()
			}
		}

	}
}

func (s *Session) removeStream(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.streams[id]; ok {
		delete(s.streams, id)
	}
}

func (s *Session) asyncPushStream(str *stream) {
	select {
	case s.streamCh <- str:
	default:
	}
}