// Copyright 2017 longXboy, longxboyhi@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	rawLog "log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/getsentry/raven-go"
	"github.com/longXboy/lunnel/contrib"
	"github.com/longXboy/lunnel/crypto"
	"github.com/longXboy/lunnel/log"
	"github.com/longXboy/lunnel/msg"
	"github.com/longXboy/lunnel/transport"
	"github.com/longXboy/lunnel/vhost"
	"github.com/longXboy/smux"
)

func Main(configDetail []byte, configType string) {
	err := LoadConfig(configDetail, configType)
	if err != nil {
		rawLog.Fatalf("load config failed!err:=%v", err)
	}
	if serverConf.LogFile != "" {
		f, err := os.OpenFile(serverConf.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0660)
		if err != nil {
			rawLog.Fatalf("open log file failed!err:=%v\n", err)
			return
		}
		defer f.Close()
		log.Init(serverConf.Debug, f)
	} else {
		log.Init(serverConf.Debug, nil)
	}
	raven.SetDSN(serverConf.DSN)
	if serverConf.AuthEnable {
		contrib.InitAuth(serverConf.AuthUrl)
	}
	if serverConf.NotifyEnable {
		contrib.InitNotify(serverConf.NotifyUrl, serverConf.NotifyKey)
	}
	maxIdlePipes, err = strconv.ParseUint(serverConf.MaxIdlePipes, 10, 64)
	if err != nil {
		log.Fatalln("max_idle_pipes must be unsigned integer")
	}
	maxStreams, err = strconv.ParseUint(serverConf.MaxStreams, 10, 64)
	if err != nil {
		log.Fatalln("max_idle_pipes must be unsigned integer")
	}

	go serveHttp(fmt.Sprintf("%s:%d", serverConf.ListenIP, serverConf.HttpPort))
	go serveHttps(fmt.Sprintf("%s:%d", serverConf.ListenIP, serverConf.HttpsPort))
	go listenAndServe("kcp")
	go listenAndServe("tcp")
	go serveManage()

	wait := make(chan struct{})
	<-wait
}

func serveManage() {
	http.HandleFunc("/tunnel", tunnelQuery)
	http.ListenAndServe(fmt.Sprintf("%s:%d", serverConf.ListenIP, serverConf.ManagePort), nil)
}

type tunnelStateReq struct {
	RemoteAddr string
}

type tunnelStateResp struct {
	Tunnels []string
}

func tunnelQuery(w http.ResponseWriter, r *http.Request) {
	content, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "req body is empty")
		return
	}
	r.Body.Close()

	var query tunnelStateReq
	err = json.Unmarshal(content, &query)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "unmarshal req body failed")
		return
	}
	var tunnelStats tunnelStateResp = tunnelStateResp{Tunnels: []string{}}
	if query.RemoteAddr != "" {
		TunnelMapLock.RLock()
		tunnel, isok := TunnelMap[query.RemoteAddr]
		TunnelMapLock.RUnlock()
		if isok {
			tunnelStats.Tunnels = append(tunnelStats.Tunnels, tunnel.tunnelConfig.PublicAddr())
		}
	} else {
		TunnelMapLock.RLock()
		for _, v := range TunnelMap {
			tunnelStats.Tunnels = append(tunnelStats.Tunnels, v.tunnelConfig.PublicAddr())
		}
		TunnelMapLock.RUnlock()
	}
	header := w.Header()
	header["Content-Type"] = []string{"application/json"}
	w.WriteHeader(http.StatusOK)
	retBody, err := json.Marshal(tunnelStats)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "marshal resp body failed")
		return
	}
	w.Write(retBody)
}

func listenAndServe(transportMode string) {
	addr := fmt.Sprintf("%s:%d", serverConf.ListenIP, serverConf.ListenPort)
	lis, err := transport.Listen(addr, transportMode)
	if err != nil {
		log.WithFields(log.Fields{"address": addr, "protocol": transportMode, "err": err}).Fatalln("server's control listen failed!")
		return
	}
	log.WithFields(log.Fields{"address": addr, "protocol": transportMode}).Infoln("server's control listen at")
	serve(lis)
}

func handleConn(conn net.Conn) {
	mType, body, err := msg.ReadMsg(conn)
	if err != nil {
		conn.Close()
		log.WithFields(log.Fields{"err": err}).Warningln("read handshake msg failed!")
		return
	}
	if mType == msg.TypeClientHello {
		clientHello := body.(*msg.ClientHello)
		if clientHello.EncryptMode == "tls" && (serverConf.Tls.TlsCert == "" || serverConf.Tls.TlsKey == "") {
			err = msg.WriteMsg(conn, msg.TypeError, msg.Error{Msg: "server not support tls mode"})
			if err != nil {
				conn.Close()
				return
			}
		} else if clientHello.EncryptMode == "aes" && serverConf.Aes.SecretKey == "" {
			err = msg.WriteMsg(conn, msg.TypeError, msg.Error{Msg: "server not support aes mode"})
			if err != nil {
				conn.Close()
				return
			}
		} else {
			err = msg.WriteMsg(conn, msg.TypeServerHello, nil)
			if err != nil {
				conn.Close()
				return
			}
		}
		var underlyingConn io.ReadWriteCloser
		var err error
		if clientHello.EncryptMode == "tls" {
			tlsConfig, err := newTlsConfig()
			if err != nil {
				conn.Close()
				return
			}
			underlyingConn = tls.Server(conn, tlsConfig)
		} else if clientHello.EncryptMode == "aes" {
			underlyingConn, err = crypto.NewCryptoStream(conn, []byte(serverConf.Aes.SecretKey))
			if err != nil {
				conn.Close()
				log.WithFields(log.Fields{"err": err}).Errorln("client hello,crypto.NewCryptoConn failed!")
				return
			}
		} else if clientHello.EncryptMode == "none" {
			underlyingConn = conn
		} else {
			msg.WriteMsg(conn, msg.TypeError, msg.Error{Msg: "invalid encryption mode"})
			conn.Close()
			log.WithFields(log.Fields{"encrypt_mode": clientHello.EncryptMode, "err": "invalid EncryptMode"}).Errorln("client hello failed!")
			return
		}
		if clientHello.EnableCompress {
			underlyingConn = transport.NewCompStream(underlyingConn)
		}
		smuxConfig := smux.DefaultConfig()
		smuxConfig.MaxReceiveBuffer = 419430
		sess, err := smux.Server(underlyingConn, smuxConfig)
		if err != nil {
			underlyingConn.Close()
			log.WithFields(log.Fields{"err": err}).Warningln("upgrade to smux.Server failed!")
			return
		}
		defer sess.Close()
		stream, err := sess.AcceptStream()
		if err != nil {
			log.WithFields(log.Fields{"err": err}).Warningln("accept stream failed!")
			return
		}
		log.WithFields(log.Fields{"encrypt_mode": body.(*msg.ClientHello).EncryptMode}).Debugln("new client hello")
		handleControl(stream, clientHello)
	} else if mType == msg.TypePipeClientHello {
		handlePipe(conn, body.(*msg.PipeClientHello))
	} else {
		log.WithFields(log.Fields{"msgType": mType, "body": body}).Errorln("read handshake msg invalid type!")
	}
}

func serve(lis net.Listener) {
	for {
		if conn, err := lis.Accept(); err == nil {
			go handleConn(conn)
		} else {
			log.WithFields(log.Fields{"err": err}).Errorln("lis.Accept failed!")
		}
	}

}

func handleHttpsConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second * 20))
	sconn, info, err := vhost.GetHttpsHostname(conn)
	if err != nil {
		log.WithFields(log.Fields{"err": err}).Debugln("vhost.GetHttpRequestInfo failed!")
		return
	}
	TunnelMapLock.RLock()
	tunnel, isok := TunnelMap[fmt.Sprintf("https://%s:%d", info["Host"], serverConf.HttpsPort)]
	TunnelMapLock.RUnlock()
	tlsConfig, err := newTlsConfig()
	if err != nil {
		log.Errorln("server error cert")
		return
	}
	tlsConn := tls.Server(sconn, tlsConfig)
	if isok {
		conn.SetDeadline(time.Time{})
		proxyConn(tlsConn, tunnel.ctl, tunnel.name)
	} else {
		tlsConn.Write([]byte(vhost.BadGateWayResp()))
	}
}

func serveHttps(addr string) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.WithFields(log.Fields{"addr": addr, "err": err}).Fatalln("listen https failed!")
	}
	log.WithFields(log.Fields{"addr": addr, "err": err}).Infoln("listen https")
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.WithFields(log.Fields{"err": err}).Errorln("accept http conn failed!")
			continue
		}
		go handleHttpsConn(conn)
	}
}

func handleHttpConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second * 20))
	sconn, info, err := vhost.GetHttpRequestInfo(conn)
	if err != nil {
		log.WithFields(log.Fields{"err": err}).Debugln("vhost.GetHttpRequestInfo failed!")
		return
	}
	TunnelMapLock.RLock()
	tunnel, isok := TunnelMap[fmt.Sprintf("http://%s:%d", info["Host"], serverConf.HttpPort)]
	TunnelMapLock.RUnlock()
	if isok {
		if tunnel.tunnelConfig.HttpHostRewrite != "" {
			sconn, err = vhost.HttpHostNameRewrite(sconn, tunnel.tunnelConfig.HttpHostRewrite)
			if err != nil {
				log.WithFields(log.Fields{"err": err}).Errorln("vhost.HttpHostNameRewrite failed!")
				return
			}
		}
		conn.SetDeadline(time.Time{})
		proxyConn(sconn, tunnel.ctl, tunnel.name)
	} else {
		sconn.Write([]byte(vhost.BadGateWayResp()))
	}
}

func serveHttp(addr string) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.WithFields(log.Fields{"addr": addr, "err": err}).Fatalln("listen http failed!")
	}
	log.WithFields(log.Fields{"addr": addr, "err": err}).Infoln("listen http")
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.WithFields(log.Fields{"err": err}).Errorln("accept http conn failed!")
			continue
		}
		go handleHttpConn(conn)
	}
}

func newTlsConfig() (*tls.Config, error) {
	var err error
	tlsConfig := &tls.Config{}
	tlsConfig.Certificates = make([]tls.Certificate, 1)
	tlsConfig.Certificates[0], err = tls.LoadX509KeyPair(serverConf.Tls.TlsCert, serverConf.Tls.TlsKey)
	if err != nil {
		log.WithFields(log.Fields{"cert": serverConf.Tls.TlsCert, "private_key": serverConf.Tls.TlsKey, "err": err}).Errorln("load LoadX509KeyPair failed!")
		return tlsConfig, err
	}
	return tlsConfig, nil
}

func handleControl(conn net.Conn, cch *msg.ClientHello) {
	ctl := NewControl(conn, cch.EncryptMode, cch.EnableCompress, cch.Version)
	err := ctl.ServerHandShake()
	if err != nil {
		conn.Close()
		log.WithFields(log.Fields{"err": err, "client_id": ctl.ClientID.String()}).Errorln("ctl.ServerHandShake failed!")
		return
	}
	log.WithFields(log.Fields{"client_id": ctl.ClientID.String(), "encrypt_mode": ctl.encryptMode, "enableCompress": ctl.enableCompress, "version": cch.Version}).Infoln("client handshake success!")
	ctl.Serve()
}

func handlePipe(conn net.Conn, phs *msg.PipeClientHello) {
	err := PipeHandShake(conn, phs)
	if err != nil {
		conn.Close()
		log.WithFields(log.Fields{"err": err}).Warningln("pipe handshake failed!")
	}
}
