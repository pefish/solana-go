// Copyright 2021 github.com/gagliardetto
// This file has been modified by github.com/gagliardetto
//
// Copyright 2020 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/buger/jsonparser"
	"github.com/gorilla/rpc/v2/json2"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

var ErrSubscriptionClosed = errors.New("subscription closed")

type result interface{}

type Client struct {
	rpcURL                  string
	conn                    *websocket.Conn
	connCtx                 context.Context
	connCtxCancel           context.CancelFunc
	lock                    sync.RWMutex
	subscriptionByRequestID map[uint64]*Subscription
	subscriptionByWSSubID   map[uint64]*Subscription
	reconnectOnErr          bool
	shortID                 bool
}

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second
)

// Connect creates a new websocket client connecting to the provided endpoint.
func Connect(ctx context.Context, rpcEndpoint string) *Client {
	return ConnectWithOptions(ctx, rpcEndpoint, nil)
}

// ConnectWithOptions creates a new websocket client connecting to the provided
// endpoint with a http header if available The http header can be helpful to
// pass basic authentication params as prescribed
// ref https://github.com/gorilla/websocket/issues/209
func ConnectWithOptions(ctx context.Context, rpcEndpoint string, opt *Options) *Client {
	c := &Client{
		rpcURL:                  rpcEndpoint,
		subscriptionByRequestID: map[uint64]*Subscription{},
		subscriptionByWSSubID:   map[uint64]*Subscription{},
	}

	dialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  DefaultHandshakeTimeout,
		EnableCompression: true,
	}

	if opt != nil && opt.ShortID {
		c.shortID = opt.ShortID
	}

	if opt != nil && opt.HandshakeTimeout > 0 {
		dialer.HandshakeTimeout = opt.HandshakeTimeout
	}

	var httpHeader http.Header = nil
	if opt != nil && opt.HttpHeader != nil && len(opt.HttpHeader) > 0 {
		httpHeader = opt.HttpHeader
	}

	var resp *http.Response
	var err error
	for {
		fmt.Println("connecting...")
		c.conn, resp, err = dialer.DialContext(ctx, rpcEndpoint, httpHeader)
		if err != nil {
			if resp != nil {
				body, _ := io.ReadAll(resp.Body)
				err = fmt.Errorf("new ws client: dial: %w, status: %s, body: %q", err, resp.Status, string(body))
			} else {
				err = fmt.Errorf("new ws client: dial: %w", err)
			}
			fmt.Printf("connect failed, to reconnect... <err: %s>\n", err.Error())
			time.Sleep(time.Second)
			continue
		}
		fmt.Println("connect success.")
		break
	}

	c.connCtx, c.connCtxCancel = context.WithCancel(context.Background())

	isReconnectChan := make(chan bool)
	isReconnetDoneChan := make(chan bool)
	go func() {
		for {
			select {
			case <-c.connCtx.Done():
				return
			case <-isReconnectChan:
				var resp *http.Response
				var err error
				for {
					fmt.Println("connecting...")
					c.conn, resp, err = dialer.DialContext(ctx, rpcEndpoint, httpHeader)
					if err != nil {
						if resp != nil {
							body, _ := io.ReadAll(resp.Body)
							err = fmt.Errorf("new ws client: dial: %w, status: %s, body: %q", err, resp.Status, string(body))
						} else {
							err = fmt.Errorf("new ws client: dial: %w", err)
						}
						fmt.Printf("connect failed, to reconnect... <err: %s>\n", err.Error())
						time.Sleep(time.Second)
						continue
					}
					fmt.Println("connect success.")
					isReconnetDoneChan <- true
					break
				}
			}
		}
	}()

	go func() {
		readDeadline := 10 * time.Second
		if opt != nil && opt.ReadDeadline != 0 {
			readDeadline = opt.ReadDeadline
		}
		c.conn.SetReadDeadline(time.Now().Add(readDeadline))
		c.conn.SetPongHandler(func(appData string) error {
			fmt.Println("pong")
			c.conn.SetReadDeadline(time.Now().Add(readDeadline))
			return nil
		})
		ticker := time.NewTicker((readDeadline * 9) / 10)
		for {
			select {
			case <-c.connCtx.Done():
				return
			case <-ticker.C:
				c.conn.SetWriteDeadline(time.Now().Add(writeWait))
				err := c.conn.WriteMessage(websocket.PingMessage, []byte{})
				if err != nil {
					fmt.Printf("ping failed, to reconnect... <err: %s>\n", err.Error())
					isReconnectChan <- true
					<-isReconnetDoneChan
					continue
				}
				fmt.Println("ping")
			}
		}
	}()

	go func() {
		for {
			select {
			case <-c.connCtx.Done():
				return
			default:
				// fmt.Printf("reading...\n")
				_, message, err := c.conn.ReadMessage()
				if err != nil {
					fmt.Printf("ReadMessage error, to reconnect... <err: %s>\n", err.Error())
					isReconnectChan <- true
					<-isReconnetDoneChan
					continue
				}
				c.handleMessage(message)
			}
		}
	}()
	return c
}

func (c *Client) Close() {
	c.connCtxCancel()
	c.conn.Close()
}

// GetUint64 returns the value retrieved by `Get`, cast to a uint64 if possible.
// If key data type do not match, it will return an error.
func getUint64(data []byte, keys ...string) (val uint64, err error) {
	v, t, _, e := jsonparser.Get(data, keys...)
	if e != nil {
		return 0, e
	}
	if t != jsonparser.Number {
		return 0, fmt.Errorf("Value is not a number: %s", string(v))
	}
	return strconv.ParseUint(string(v), 10, 64)
}

func getUint64WithOk(data []byte, path ...string) (uint64, bool) {
	val, err := getUint64(data, path...)
	if err == nil {
		return val, true
	}
	return 0, false
}

func (c *Client) handleMessage(message []byte) {
	// when receiving message with id. the result will be a subscription number.
	// that number will be associated to all future message destine to this request

	requestID, ok := getUint64WithOk(message, "id")
	if ok {
		subID, _ := getUint64WithOk(message, "result")
		c.handleNewSubscriptionMessage(requestID, subID)
		return
	}

	subID, _ := getUint64WithOk(message, "params", "subscription")
	c.handleSubscriptionMessage(subID, message)
}

func (c *Client) handleNewSubscriptionMessage(requestID, subID uint64) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if traceEnabled {
		zlog.Debug("received new subscription message",
			zap.Uint64("message_id", requestID),
			zap.Uint64("subscription_id", subID),
		)
	}

	callBack, found := c.subscriptionByRequestID[requestID]
	if !found {
		zlog.Error("cannot find websocket message handler for a new stream.... this should not happen",
			zap.Uint64("request_id", requestID),
			zap.Uint64("subscription_id", subID),
		)
		return
	}
	callBack.subID = subID
	c.subscriptionByWSSubID[subID] = callBack

	zlog.Debug("registered ws subscription",
		zap.Uint64("subscription_id", subID),
		zap.Uint64("request_id", requestID),
		zap.Int("subscription_count", len(c.subscriptionByWSSubID)),
	)
	return
}

func (c *Client) handleSubscriptionMessage(subID uint64, message []byte) {
	if traceEnabled {
		zlog.Debug("received subscription message",
			zap.Uint64("subscription_id", subID),
		)
	}

	c.lock.RLock()
	sub, found := c.subscriptionByWSSubID[subID]
	c.lock.RUnlock()
	if !found {
		zlog.Warn("unable to find subscription for ws message", zap.Uint64("subscription_id", subID))
		return
	}

	// Decode the message using the subscription-provided decoderFunc.
	result, err := sub.decoderFunc(message)
	if err != nil {
		fmt.Println("*****************************")
		c.closeSubscription(sub.req.ID, fmt.Errorf("unable to decode client response: %w", err))
		return
	}

	// this cannot be blocking or else
	// we  will no read any other message
	if len(sub.stream) >= cap(sub.stream) {
		zlog.Warn("closing ws client subscription... not consuming fast en ought",
			zap.Uint64("request_id", sub.req.ID),
		)
		c.closeSubscription(sub.req.ID, fmt.Errorf("reached channel max capacity %d", len(sub.stream)))
		return
	}

	if !sub.closed {
		sub.stream <- result
	}
	return
}

func (c *Client) closeAllSubscription(err error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	for _, sub := range c.subscriptionByRequestID {
		sub.err <- err
	}

	c.subscriptionByRequestID = map[uint64]*Subscription{}
	c.subscriptionByWSSubID = map[uint64]*Subscription{}
}

func (c *Client) closeSubscription(reqID uint64, err error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	sub, found := c.subscriptionByRequestID[reqID]
	if !found {
		return
	}

	sub.err <- err

	err = c.unsubscribe(sub.subID, sub.unsubscribeMethod)
	if err != nil {
		zlog.Warn("unable to send rpc unsubscribe call",
			zap.Error(err),
		)
	}

	delete(c.subscriptionByRequestID, sub.req.ID)
	delete(c.subscriptionByWSSubID, sub.subID)
}

func (c *Client) unsubscribe(subID uint64, method string) error {
	req := newRequest([]interface{}{subID}, method, nil, c.shortID)
	data, err := req.encode()
	if err != nil {
		return fmt.Errorf("unable to encode unsubscription message for subID %d and method %s", subID, method)
	}

	c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	err = c.conn.WriteMessage(websocket.TextMessage, data)
	if err != nil {
		return fmt.Errorf("unable to send unsubscription message for subID %d and method %s", subID, method)
	}
	return nil
}

func (c *Client) subscribe(
	params []interface{},
	conf map[string]interface{},
	subscriptionMethod string,
	unsubscribeMethod string,
	decoderFunc decoderFunc,
) (*Subscription, error) {
	fmt.Printf("subscrib\n")
	c.lock.Lock()
	defer c.lock.Unlock()

	req := newRequest(params, subscriptionMethod, conf, c.shortID)
	data, err := req.encode()
	if err != nil {
		return nil, fmt.Errorf("subscribe: unable to encode subsciption request: %w", err)
	}

	sub := newSubscription(
		req,
		func(err error) {
			c.closeSubscription(req.ID, err)
		},
		unsubscribeMethod,
		decoderFunc,
	)

	c.subscriptionByRequestID[req.ID] = sub
	zlog.Info("added new subscription to websocket client", zap.Int("count", len(c.subscriptionByRequestID)))

	zlog.Debug("writing data to conn", zap.String("data", string(data)))
	c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	fmt.Printf("subscribing...\n")
	err = c.conn.WriteMessage(websocket.TextMessage, data)
	if err != nil {
		delete(c.subscriptionByRequestID, req.ID)
		return nil, fmt.Errorf("unable to write request: %w", err)
	}
	fmt.Printf("subscribe success\n")

	return sub, nil
}

func decodeResponseFromReader(r io.Reader, reply interface{}) (err error) {
	var c *response
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return err
	}

	if c.Error != nil {
		jsonErr := &json2.Error{}
		if err := json.Unmarshal(*c.Error, jsonErr); err != nil {
			return &json2.Error{
				Code:    json2.E_SERVER,
				Message: string(*c.Error),
			}
		}
		return jsonErr
	}

	if c.Params == nil {
		return json2.ErrNullResult
	}

	return json.Unmarshal(*c.Params.Result, &reply)
}

func decodeResponseFromMessage(r []byte, reply interface{}) (err error) {
	var c *response
	if err := json.Unmarshal(r, &c); err != nil {
		return err
	}

	if c.Error != nil {
		jsonErr := &json2.Error{}
		if err := json.Unmarshal(*c.Error, jsonErr); err != nil {
			return &json2.Error{
				Code:    json2.E_SERVER,
				Message: string(*c.Error),
			}
		}
		return jsonErr
	}

	if c.Params == nil {
		return json2.ErrNullResult
	}

	return json.Unmarshal(*c.Params.Result, &reply)
}
