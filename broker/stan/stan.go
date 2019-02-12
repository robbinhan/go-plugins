// Package stan provides a NATS Streaming broker
package stan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/micro/go-log"
	"github.com/micro/go-micro/broker"
	"github.com/micro/go-micro/cmd"
	"github.com/micro/go-micro/codec/json"
	stan "github.com/nats-io/go-nats-streaming"
)

type stanBroker struct {
	sync.RWMutex
	addrs     []string
	conn      stan.Conn
	opts      broker.Options
	sopts     stan.Options
	nopts     []stan.Option
	clusterID string
	timeout   time.Duration
	reconnect bool
	done      chan struct{}
	ctx       context.Context
}

type subscriber struct {
	t    string
	s    stan.Subscription
	dq   bool
	opts broker.SubscribeOptions
}

type publication struct {
	t   string
	msg *stan.Msg
	m   *broker.Message
}

func init() {
	cmd.DefaultBrokers["stan"] = NewBroker
}

func (n *publication) Topic() string {
	return n.t
}

func (n *publication) Message() *broker.Message {
	return n.m
}

func (n *publication) Ack() error {
	return n.msg.Ack()
}

func (n *subscriber) Options() broker.SubscribeOptions {
	return n.opts
}

func (n *subscriber) Topic() string {
	return n.t
}

func (n *subscriber) Unsubscribe() error {
	if n.s == nil {
		return nil
	}
	// go-micro server Unsubscribe can't handle durable queues, so close as stan suggested
	// from nats streaming readme:
	// When a client disconnects, the streaming server is not notified, hence the importance of calling Close()
	if !n.dq {
		err := n.s.Unsubscribe()
		if err != nil {
			return err
		}
	}
	return n.Close()
}

func (n *subscriber) Close() error {
	if n.s != nil {
		return n.s.Close()
	}
	return nil
}

func (n *stanBroker) Address() string {
	// stan does not support connected server info
	if len(n.addrs) > 0 {
		return n.addrs[0]
	}

	return ""
}

func setAddrs(addrs []string) []string {
	var cAddrs []string
	for _, addr := range addrs {
		if len(addr) == 0 {
			continue
		}
		if !strings.HasPrefix(addr, "nats://") {
			addr = "nats://" + addr
		}
		cAddrs = append(cAddrs, addr)
	}
	if len(cAddrs) == 0 {
		cAddrs = []string{stan.DefaultNatsURL}
	}
	return cAddrs
}

func (n *stanBroker) reconnectCB(c stan.Conn, err error) {
	if n.reconnect {
		if err := n.connect(); err != nil {
			log.Log(err.Error())
		}
	}
}

func (n *stanBroker) connect() error {
	timeout := make(<-chan time.Time)

	if n.timeout > 0 {
		timeout = time.After(n.timeout)
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	fn := func() error {
		clientID := uuid.New().String()
		c, err := stan.Connect(n.clusterID, clientID, n.nopts...)
		if err == nil {
			n.Lock()
			n.conn = c
			n.Unlock()
		}
		return err
	}

	// don't wait for first try
	if err := fn(); err == nil {
		return nil
	}

	// wait loop
	for {
		select {
		// context closed
		case <-n.opts.Context.Done():
			return nil
		// call close, don't wait anymore
		case <-n.done:
			return nil
		//  in case of timeout fail with a timeout error
		case <-timeout:
			return fmt.Errorf("timeout connect to %v", n.addrs)
		// got a tick, try to connect
		case <-ticker.C:
			err := fn()
			if err == nil {
				log.Logf("successeful connected to %v", n.addrs)
				return nil
			}
			log.Logf("failed to connect %v: %v\n", n.addrs, err)
		}
	}

	return nil
}

func (n *stanBroker) Connect() error {
	n.RLock()
	if n.conn != nil {
		n.RUnlock()
		return nil
	}
	n.RUnlock()

	clusterID, ok := n.opts.Context.Value(clusterIDKey{}).(string)
	if !ok || len(clusterID) == 0 {
		return errors.New("must specify ClusterID Option")
	}

	var reconnect bool
	if val, ok := n.opts.Context.Value(reconnectKey{}).(bool); ok && val {
		reconnect = val
	}

	var timeout time.Duration
	if td, ok := n.opts.Context.Value(timeoutKey{}).(time.Duration); ok {
		timeout = td
	} else {
		timeout = 5 * time.Second
	}

	if n.sopts.ConnectionLostCB != nil && reconnect {
		return errors.New("impossible to use custom ConnectionLostCB and Reconnect(true)")
	}

	if reconnect {
		n.sopts.ConnectionLostCB = n.reconnectCB
	}

	nopts := []stan.Option{
		stan.NatsURL(n.sopts.NatsURL),
		stan.NatsConn(n.sopts.NatsConn),
		stan.ConnectWait(n.sopts.ConnectTimeout),
		stan.PubAckWait(n.sopts.AckTimeout),
		stan.MaxPubAcksInflight(n.sopts.MaxPubAcksInflight),
		stan.Pings(n.sopts.PingIterval, n.sopts.PingMaxOut),
		stan.SetConnectionLostHandler(n.sopts.ConnectionLostCB),
	}
	nopts = append(nopts, stan.NatsURL(strings.Join(n.addrs, ",")))

	n.Lock()
	n.nopts = nopts
	n.clusterID = clusterID
	n.timeout = timeout
	n.reconnect = reconnect
	n.Unlock()

	return n.connect()
}

func (n *stanBroker) Disconnect() error {
	var err error

	n.Lock()
	defer n.Unlock()

	if n.done != nil {
		close(n.done)
		n.done = nil
	}
	if n.conn != nil {
		err = n.conn.Close()
	}
	return err
}

func (n *stanBroker) Init(opts ...broker.Option) error {
	for _, o := range opts {
		o(&n.opts)
	}
	n.addrs = setAddrs(n.opts.Addrs)
	return nil
}

func (n *stanBroker) Options() broker.Options {
	return n.opts
}

func (n *stanBroker) Publish(topic string, msg *broker.Message, opts ...broker.PublishOption) error {
	b, err := n.opts.Codec.Marshal(msg)
	if err != nil {
		return err
	}
	n.RLock()
	defer n.RUnlock()
	return n.conn.Publish(topic, b)
}

func (n *stanBroker) Subscribe(topic string, handler broker.Handler, opts ...broker.SubscribeOption) (broker.Subscriber, error) {
	if n.conn == nil {
		return nil, errors.New("not connected")
	}
	var ackSuccess bool

	opt := broker.SubscribeOptions{
		AutoAck: true,
	}

	for _, o := range opts {
		o(&opt)
	}

	// Make sure context is setup
	if opt.Context == nil {
		opt.Context = context.Background()
	}

	ctx := opt.Context
	if subscribeContext, ok := ctx.Value(subscribeContextKey{}).(context.Context); ok && subscribeContext != nil {
		ctx = subscribeContext
	}

	var stanOpts []stan.SubscriptionOption
	if !opt.AutoAck {
		stanOpts = append(stanOpts, stan.SetManualAckMode())
	}

	if subOpts, ok := ctx.Value(subscribeOptionKey{}).([]stan.SubscriptionOption); ok && len(subOpts) > 0 {
		stanOpts = append(stanOpts, subOpts...)
	}

	if bval, ok := ctx.Value(ackSuccessKey{}).(bool); ok && bval {
		stanOpts = append(stanOpts, stan.SetManualAckMode())
		ackSuccess = true
	}

	bopts := stan.DefaultSubscriptionOptions
	for _, bopt := range stanOpts {
		if err := bopt(&bopts); err != nil {
			return nil, err
		}
	}

	opt.AutoAck = !bopts.ManualAcks

	fn := func(msg *stan.Msg) {
		var m broker.Message

		// unmarshal message
		if err := n.opts.Codec.Unmarshal(msg.Data, &m); err != nil {
			return
		}

		// execute the handler
		err := handler(&publication{m: &m, msg: msg, t: msg.Subject})
		// if there's no error and success auto ack is enabled ack it
		if err == nil && ackSuccess {
			msg.Ack()
		}
	}

	var sub stan.Subscription
	var err error

	n.RLock()
	if len(opt.Queue) > 0 {
		sub, err = n.conn.QueueSubscribe(topic, opt.Queue, fn, stanOpts...)
	} else {
		sub, err = n.conn.Subscribe(topic, fn, stanOpts...)
	}
	n.RUnlock()
	if err != nil {
		return nil, err
	}
	return &subscriber{dq: len(bopts.DurableName) > 0, s: sub, opts: opt, t: topic}, nil
}

func (n *stanBroker) String() string {
	return "stan"
}

func NewBroker(opts ...broker.Option) broker.Broker {
	options := broker.Options{
		// Default codec
		Codec:   json.Marshaler{},
		Context: context.Background(),
	}

	for _, o := range opts {
		o(&options)
	}

	stanOpts := stan.DefaultOptions
	if n, ok := options.Context.Value(optionsKey{}).(stan.Options); ok {
		stanOpts = n
	}

	if len(options.Addrs) == 0 {
		options.Addrs = strings.Split(stanOpts.NatsURL, ",")
	}

	nb := &stanBroker{
		done:  make(chan struct{}),
		opts:  options,
		sopts: stanOpts,
		addrs: setAddrs(options.Addrs),
	}

	return nb
}