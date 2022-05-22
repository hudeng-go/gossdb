//Package ssdbclient Establish a connection with SSDB, parse the data and convert it into a regular format
//
//与ssdb建立连接，对数据进行解析，转换成常规格式
package ssdbclient

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/seefan/goerr"
	"github.com/seefan/gossdb/v2/conf"
)

const (
	endN = '\n'
	endR = '\r'
	oK   = "ok"
)

//NewSSDBClient create new ssdb client
//
//  @param cfg initial parameters
//  @return new *SSDBClient
//
//使用配置创建一个新的SSDBClient，并不实际打开连接
func NewSSDBClient(cfg *conf.Config) *SSDBClient {
	return &SSDBClient{
		host:            cfg.Host,
		port:            cfg.Port,
		password:        cfg.Password,
		readTimeout:     cfg.ReadTimeout,
		writeTimeout:    cfg.WriteTimeout,
		readBufferSize:  cfg.ReadBufferSize,
		writeBufferSize: cfg.WriteBufferSize,
		retryEnabled:    cfg.RetryEnabled,
		connectTimeout:  cfg.ConnectTimeout,
		encoding:        cfg.Encoding,
	}
}

//SSDBClient ssdb client
type SSDBClient struct {
	//是否重试
	retryEnabled bool
	//是否自动转码
	encoding bool
	//whether the connection is open
	isOpen bool
	////whether the connection is authorization
	//isAuth bool
	//packetBuf bytes.Buffer
	//连接写缓冲，默认为8k，单位为kb
	writeBufferSize int
	//连接读缓冲，默认为8k，单位为kb
	readBufferSize int
	//写超时
	writeTimeout int
	//读超时
	readTimeout int
	//创建连接的超时时间，单位为秒。默认值: 5
	connectTimeout int
	//ssdb port
	port int
	//bufSize
	bufSize int
	//tmpSize
	offset int
	//0时间
	timeZero time.Time
	//connection token
	password string
	//host ssdb host
	host string
	//connection
	sock *net.TCPConn
	//readBuf
	buf []byte
	//The input parameter is converted to [] bytes, which by default is converted to json format
	//and can be modified to use a custom serialization
	//将输入参数成[]byte，默认会转换成json格式,可以修改这个参数以便使用自定义的序列化方式
	EncodingFunc func(v interface{}) []byte
	//dialer
	dialer   *net.Dialer
	recv_buf bytes.Buffer
}

//Start start socket
//
//  @return error that may occur on startup. Return nil if successful startup
//
//启动连接，并设置读写的缓存
func (s *SSDBClient) Start() error {
	if s.dialer == nil {
		s.dialer = &net.Dialer{Timeout: time.Second * time.Duration(s.connectTimeout)}
	}
	conn, err := s.dialer.Dial("tcp", fmt.Sprintf("%s:%d", s.host, s.port))
	if err != nil {
		return err
	}
	sock := conn.(*net.TCPConn)
	err = sock.SetReadBuffer(s.readBufferSize * 1024)
	if err != nil {
		return err
	}
	err = sock.SetWriteBuffer(s.writeBufferSize * 1024)
	if err != nil {
		return err
	}
	s.bufSize = s.writeBufferSize * 1024
	s.buf = make([]byte, s.bufSize)
	s.sock = sock
	s.timeZero = time.Time{}
	s.isOpen = true
	return s.auth()
}

//Close close SSDBClient
//
//  @return error that may occur on shutdown. Return nil if successful shutdown
func (s *SSDBClient) Close() error {
	s.isOpen = false
	s.buf = nil
	if s.sock == nil {
		return nil
	}

	return s.sock.Close()
}

//IsOpen check if the connection is open
//
//  @return bool returns true if the connection is open
//
//是否为打开状态
func (s *SSDBClient) IsOpen() bool {
	return s.isOpen
}

//执行ssdb命令
func (s *SSDBClient) do(args ...interface{}) (resp []string, err error) {
	if !s.isOpen {
		return nil, goerr.String("gossdb client is closed.")
	}
	defer func() {
		if e := recover(); e != nil {
			s.isOpen = false
			err = fmt.Errorf("%v", e)
		}
	}()
	if err = s.send(args); err != nil {
		s.isOpen = false
		return nil, goerr.Errorf(err, "client send error")
	}
	if resp, err = s.recv(); err != nil {
		s.isOpen = false
		return nil, goerr.Errorf(err, "client recv error")
	}
	return
}

func (s *SSDBClient) auth() error {
	if s.password == "" { //without a password, authentication is not required
		return nil
	}
	//if !s.isAuth {
	resp, err := s.do("auth", s.password)
	if err != nil {
		if e := s.Close(); e != nil {
			err = goerr.Errorf(err, "client close failed")
		}
		return goerr.Errorf(err, "authentication failed")
	}
	if len(resp) > 0 && resp[0] == oK {
		//验证成功
		//s.isAuth = true
		return nil
	}
	return goerr.String("authentication failed,password is wrong")

	//}
	//return nil
}

//Do common function
//
//  @param args the input parameters
//  @return []string output parameters
//  @return error Possible errors
//
//通用调用方法，所有操作ssdb的函数最终都是调用这个函数
func (s *SSDBClient) Do(args ...interface{}) ([]string, error) {
	//if err := s.auth(); err != nil {
	//	return nil, err
	//}
	resp, err := s.do(args...)
	if err != nil {
		if e := s.Close(); e != nil {
			err = goerr.Errorf(err, "client close failed")
		}
		if s.retryEnabled { //如果允许重试，就重新打开一次连接
			if err = s.Start(); err == nil {
				resp, err = s.do(args...)
				if err != nil {
					if e := s.Close(); e != nil {
						err = goerr.Errorf(err, "client close failed")
					}
				}
			}
		}
	}
	return resp, err
}

//write write to buf
func (s *SSDBClient) writeBytes(bs []byte) error {
	lbs := strconv.AppendInt(nil, int64(len(bs)), 10)
	if err := s.write(lbs, len(lbs)); err != nil {
		return err
	}
	if err := s.write([]byte{endN}, 1); err != nil {
		return err
	}
	if err := s.write(bs, len(bs)); err != nil {
		return err
	}
	if err := s.write([]byte{endN}, 1); err != nil {
		return err
	}
	return nil
}
func (s *SSDBClient) write(bs []byte, size int) error {
	if s.offset+size < s.bufSize {
		copy(s.buf[s.offset:], bs)
		s.offset += size
		return nil
	}
	err := s.writeSocket(s.buf[:s.offset], s.offset)
	if err != nil {
		return err
	}
	s.offset = 0
	err = s.writeSocket(bs, size)
	if err != nil {
		return err
	}
	return nil
}
func (s *SSDBClient) writeSocket(bs []byte, size int) error {
	wn := 0
	for {
		n, err := s.sock.Write(bs)
		if err != nil {
			return err
		}
		if n == size || wn+n == size {
			return nil
		}
		wn += n
		runtime.Gosched()
	}
}

//send cmd to ssdb
func (s *SSDBClient) send(args []interface{}) (err error) {
	if err = s.sock.SetWriteDeadline(time.Now().Add(time.Second * time.Duration(s.writeTimeout))); err != nil {
		return err
	}
	s.offset = 0
	for _, arg := range args {
		switch arg := arg.(type) {
		case string:
			err = s.writeBytes([]byte(arg))
		case []byte:
			err = s.writeBytes(arg)
		case int:
			bs := strconv.AppendInt(nil, int64(arg), 10)
			err = s.writeBytes(bs)
		case int8:
			err = s.writeBytes([]byte{byte(arg)})
		case int16:
			bs := strconv.AppendInt(nil, int64(arg), 10)
			err = s.writeBytes(bs)
		case int32:
			bs := strconv.AppendInt(nil, int64(arg), 10)
			err = s.writeBytes(bs)
		case int64:
			bs := strconv.AppendInt(nil, arg, 10)
			err = s.writeBytes(bs)
		case uint8:
			err = s.writeBytes([]byte{byte(arg)})
		case uint16:
			bs := strconv.AppendUint(nil, uint64(arg), 10)
			err = s.writeBytes(bs)
		case uint32:
			bs := strconv.AppendUint(nil, uint64(arg), 10)
			err = s.writeBytes(bs)
		case uint64:
			bs := strconv.AppendUint(nil, arg, 10)
			err = s.writeBytes(bs)
		case float32:
			bs := strconv.AppendFloat(nil, float64(arg), 'g', -1, 32)
			err = s.writeBytes(bs)
		case float64:
			bs := strconv.AppendFloat(nil, arg, 'g', -1, 64)
			err = s.writeBytes(bs)
		case bool:
			if arg {
				err = s.writeBytes([]byte{'1'})
			} else {
				err = s.writeBytes([]byte{'0'})
			}
		case time.Time:
			bs := strconv.AppendInt(nil, arg.Unix(), 10)
			err = s.writeBytes(bs)
		case time.Duration:
			bs := strconv.AppendInt(nil, arg.Nanoseconds(), 10)
			err = s.writeBytes(bs)
		case nil:
			err = s.writeBytes([]byte{})
		default:
			if s.encoding && s.EncodingFunc != nil {
				err = s.writeBytes(s.EncodingFunc(arg))
			} else {
				return errors.New("bad arguments type")
			}
		}
		if err != nil {
			return err
		}
	}
	if s.offset < s.bufSize {
		s.buf[s.offset] = endN
		s.offset++
		if s.offset < s.bufSize {
			err = s.writeSocket(s.buf[:s.offset], s.offset)
		} else {
			err = s.writeSocket(s.buf[:], s.bufSize)
		}
	} else {
		if err = s.writeSocket(s.buf[:s.offset], s.offset); err == nil {
			err = s.writeSocket([]byte{endN}, 1)
		}
	}

	if err != nil {
		return err
	}

	//设置不超时
	if err = s.sock.SetWriteDeadline(s.timeZero); err != nil {
		return err
	}

	return nil
}

// func recv...
func (s *SSDBClient) recv() (resp []string, err error) {
	if err = s.sock.SetReadDeadline(time.Now().Add(time.Second * time.Duration(s.readTimeout))); err != nil {
		return nil, err
	}

	tmp := make([]byte, s.readBufferSize)
	s.recv_buf.Reset()
	for {
		resp, err := s.parse()
		if resp == nil || len(resp) > 0 {
			s.sock.SetReadDeadline(s.timeZero)
			return resp, err
		}
		n, err := s.sock.Read(tmp[0:])
		if err != nil {
			s.sock.SetReadDeadline(s.timeZero)
			return nil, err
		}
		s.recv_buf.Write(tmp[0:n])
	}
}

func logs(v ...interface{}) {
	/* if os.Getenv("GOSSDB_LOG") == "" {
		return
	} */

	file, err := os.OpenFile("/tmp/gossdb.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Warn: gossdb log failed: %v", err)
	}
	defer file.Close()

	log.SetOutput(file)
	log.Println(v...)
}

//func parse...
func (s *SSDBClient) parse() ([]string, error) {
	resp := []string{}
	buf := s.recv_buf.Bytes()
	var idx, offset int
	idx = 0
	offset = 0

	for {
		idx = bytes.IndexByte(buf[offset:], endN)
		if idx == -1 {
			break
		}
		p := buf[offset : offset+idx]
		offset += idx + 1
		if len(p) == 0 || (len(p) == 1 && p[0] == endR) {
			if len(resp) == 0 {
				continue
			} else {
				var new_buf bytes.Buffer
				new_buf.Write(buf[offset:])
				s.recv_buf = new_buf
				return resp, nil
			}
		}

		size, err := strconv.Atoi(string(p))
		if err != nil || size < 0 {
			logs(string(p), " atoi got error: ", err, "resp: ", resp)
			return nil, err
		}
		if offset+size >= s.recv_buf.Len() {
			break
		}

		v := buf[offset : offset+size]
		resp = append(resp, string(v))
		offset += size + 1
	}

	//fmt.Printf("buf.size: %d packet not ready...\n", len(buf))
	return []string{}, nil
}
