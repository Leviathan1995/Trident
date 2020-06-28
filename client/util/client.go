package client

import (
	"crypto/tls"
	"log"
	"net"
	"sync"
	"time"

	"github.com/leviathan1995/Trident/encryption"
	"github.com/leviathan1995/Trident/service"
)

const maxBlock = 5000

type block struct {
	item map[string]int
	mu    *sync.RWMutex
}

type client struct {
	*service.Service
	conf              *tls.Config
	enableBypass      bool
	*block
	blockIP           []string
}

func NewClient(listen string, srvAdders []string, proxyIP []string, password string, enableBypass bool) *client {
	c := encryption.NewCipher([]byte(password))
	listenAddr, _ := net.ResolveTCPAddr("tcp", listen)

	var proxyAdders []*net.TCPAddr
	for _, srvAddr := range srvAdders {
		addr, _ := net.ResolveTCPAddr("tcp", srvAddr)
		proxyAdders = append(proxyAdders, addr)
	}
	return &client {
		&service.Service{
			Cipher:      c,
			ListenAddr:  listenAddr,
			ServerAdders: proxyAdders,
			StableProxy: proxyAdders[0],
		},
		nil,
		enableBypass,
		&block{
			mu:		&sync.RWMutex{},
			item: 	make(map[string]int),
		},
		proxyIP,
	}
}

func (c *client) Listen() error {
	for _, srv := range c.ServerAdders {
		log.Printf("Server监听地址: %s:%d", srv.IP, srv.Port)
	}
	log.Printf("默认Server监听地址: %s:%d", c.Service.StableProxy.IP, c.Service.StableProxy.Port)

	listener, err := net.ListenTCP("tcp", c.ListenAddr)
	if err != nil {
		return err
	}
	log.Printf("Client启动成功, 监听地址: %s:%d, 密码: %s", c.ListenAddr.IP, c.ListenAddr.Port, c.Cipher.Password)

	defer listener.Close()

	for {
		userConn, err := listener.AcceptTCP()
		if err != nil {
			log.Println(err)
			continue
		}
		/* Discard any unsent or unacknowledged data. */
		userConn.SetLinger(0)
		go c.handleConn(userConn)
	}
}

var srvPool = make(chan *net.TCPConn, 10)

func init() {
	go func() {
		for range time.Tick(5 * time.Second) {
			p := <-srvPool	/* Discard the idle connection */
			p.Close()
		}
	}()
}

func (c *client) newSrvConn() (*net.TCPConn, error) {
	if len(srvPool) < 10 {
		go func() {
			for i := 0; i < 2; i++ {
				proxy, err := c.DialSrv()
				if err != nil {
					log.Println(err)
					return
				}
				srvPool <- proxy
			}
		}()
	}

	select {
	case pc := <-srvPool:
		return pc, nil
	case <-time.After(100 * time.Millisecond):
		return c.DialSrv()
	}
}

func (c *client) directDial(userConn *net.TCPConn, dstAddr *net.TCPAddr) (*net.TCPConn, error){
	conn, errDial := net.DialTimeout("tcp", dstAddr.String(), time.Millisecond * 300)

	if errDial != nil {
		return &net.TCPConn{}, errDial
	} else {
		defer conn.Close()
		dstConn, errDialTCP := net.DialTCP("tcp", nil, dstAddr)
		if errDialTCP != nil {
			return &net.TCPConn{}, errDial
		} else {
			dstConn.SetLinger(0)
			/* If connect to the dst addr success, we need to notify client. */
			errDialTCP = c.TCPWrite(userConn, []byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		}
		return dstConn, errDialTCP
	}
}

func (c *client) directConnect(userConn *net.TCPConn, dstConn *net.TCPConn) {
	go func() {
		err := c.Transfer(userConn, dstConn)
		if err != nil {
			userConn.Close()
			dstConn.Close()
		}
	}()
	c.Transfer(dstConn, userConn)
}

func (c *client) searchBlockList(ip string) bool {
	c.block.mu.RLock()
	defer c.block.mu.RUnlock()

	if _, ok := c.block.item[ip]; ok {
		return true
	} else{
		return false
	}
}

func (c *client) addBlockList(ip string) {
	c.block.mu.Lock()
	defer c.block.mu.Unlock()

	if len(c.block.item) > maxBlock {
		for ip := range c.block.item {
			delete(c.block.item, ip)
			break
		}
	}
	c.block.item[ip] = 1
}

func (c *client) tryProxy(userConn *net.TCPConn, lastUserRequest []byte) {
	proxy, err := c.newSrvConn()
	if err != nil {
		log.Println(err)
		proxy, err = c.newSrvConn()
		if err != nil {
			log.Println(err)
			return
		}
	}
	defer proxy.Close()

	proxy.SetLinger(0)

	_, errWrite := c.EncodeTo(lastUserRequest, proxy)
	if errWrite != nil {
		return
	}

	go func() {
		err := c.TransferForEncode(userConn, proxy)
		if err != nil {
			userConn.Close()
			proxy.Close()
		}
	}()
	_ = c.TransferForDecode(proxy, userConn)
}

func (c *client) handleConn(userConn *net.TCPConn) {
	defer userConn.Close()

	/*  Why use lastUserRequest?
	 *  If we can not direct connect to the destination address, We need to forward
	 *  the last data package to the server.
	 */
	dstAddr, lastUserRequest, errParse := c.ParseSOCKS5(userConn)
	if errParse != nil {
		log.Printf(errParse.Error())
		return
	}

	block := c.searchBlockList(dstAddr.IP.String())
	if block {
		log.Printf("Can't directly connect to %s, Try to use Proxy", dstAddr.String())
		c.tryProxy(userConn, lastUserRequest)
	} else {
		for _, ip := range c.blockIP {
			if ip == dstAddr.IP.String() {
				go c.addBlockList(dstAddr.IP.String())
				c.tryProxy(userConn, lastUserRequest)
				return
			}
		}

		dstConn, errDirect := c.directDial(userConn, dstAddr)
		if errDirect != nil {
			log.Printf("Can't directly connect to %s, Try to use Proxy and put it into IP blacklist", dstAddr.String())
			go c.addBlockList(dstAddr.IP.String())
			c.tryProxy(userConn, lastUserRequest)
		} else {
			log.Printf("Directly connect to %s", dstAddr.String())
			c.directConnect(userConn, dstConn)
		}
	}
}