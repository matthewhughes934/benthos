package amqp1

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/benthosdev/benthos/v4/internal/component"
	"github.com/benthosdev/benthos/v4/public/service"
)

func amqp1InputSpec() *service.ConfigSpec {
	return service.NewConfigSpec().
		Stable().
		Categories("Services").
		Summary("Reads messages from an AMQP (1.0) server.").
		Description(`
### Metadata

This input adds the following metadata fields to each message:

`+"``` text"+`
- amqp_content_type
- amqp_content_encoding
- amqp_creation_time
- All string typed message annotations
`+"```"+`

You can access these metadata fields using [function interpolation](/docs/configuration/interpolation#bloblang-queries).`).
		Fields(
			service.NewURLField(urlField).
				Description("A URL to connect to.").
				Example("amqp://localhost:5672/").
				Example("amqps://guest:guest@localhost:5672/"),
			service.NewStringField(sourceAddrField).
				Description("The source address to consume from.").
				Example("/foo").
				Example("queue:/bar").
				Example("topic:/baz"),
			service.NewBoolField(azureRenewLockField).
				Description("Experimental: Azure service bus specific option to renew lock if processing takes more then configured lock time").
				Version("3.45.0").
				Default(false).
				Advanced(),
			service.NewTLSToggledField(tlsField),
			saslFieldSpec(),
		)
}

func init() {
	err := service.RegisterBatchInput("amqp_1", amqp1InputSpec(),
		func(conf *service.ParsedConfig, mgr *service.Resources) (service.BatchInput, error) {
			return amqp1ReaderFromParsed(conf, mgr)
		})
	if err != nil {
		panic(err)
	}
}

//------------------------------------------------------------------------------

type amqp1Reader struct {
	url        string
	sourceAddr string
	renewLock  bool
	connOpts   []amqp.ConnOption
	log        *service.Logger

	m    sync.RWMutex
	conn *amqp1Conn
}

func amqp1ReaderFromParsed(conf *service.ParsedConfig, mgr *service.Resources) (*amqp1Reader, error) {
	a := amqp1Reader{
		log: mgr.Logger(),
	}

	var err error
	if a.url, err = conf.FieldString(urlField); err != nil {
		return nil, err
	}

	if a.sourceAddr, err = conf.FieldString(sourceAddrField); err != nil {
		return nil, err
	}

	if a.renewLock, err = conf.FieldBool(azureRenewLockField); err != nil {
		return nil, err
	}

	if a.connOpts, err = saslOptFnsFromParsed(conf); err != nil {
		return nil, err
	}

	tlsConf, enabled, err := conf.FieldTLSToggled(tlsField)
	if err != nil {
		return nil, err
	}
	if enabled {
		a.connOpts = append(a.connOpts, amqp.ConnTLS(true), amqp.ConnTLSConfig(tlsConf))
	}

	return &a, nil
}

func (a *amqp1Reader) Connect(ctx context.Context) (err error) {
	a.m.Lock()
	defer a.m.Unlock()

	if a.conn != nil {
		return
	}

	conn := &amqp1Conn{
		log:                    a.log,
		lockRenewAddressPrefix: randomString(15),
	}

	// Create client
	if conn.client, err = amqp.Dial(a.url, a.connOpts...); err != nil {
		return
	}

	// Open a session
	if conn.session, err = conn.client.NewSession(); err != nil {
		_ = conn.Close(ctx)
		return
	}

	// Create a receiver
	if conn.receiver, err = conn.session.NewReceiver(
		amqp.LinkSourceAddress(a.sourceAddr),
		amqp.LinkCredit(10),
	); err != nil {
		_ = conn.Close(ctx)
		return
	}

	if a.renewLock {
		managementAddress := a.sourceAddr + "/$management"

		if conn.renewLockSender, err = conn.session.NewSender(
			amqp.LinkSourceAddress(conn.lockRenewAddressPrefix+lockRenewRequestSuffix),
			amqp.LinkTargetAddress(managementAddress),
		); err != nil {
			_ = conn.Close(ctx)
			return
		}
		if conn.renewLockReceiver, err = conn.session.NewReceiver(
			amqp.LinkSourceAddress(managementAddress),
			amqp.LinkTargetAddress(conn.lockRenewAddressPrefix+lockRenewResponseSuffix),
		); err != nil {
			_ = conn.Close(ctx)
			return
		}
	}

	a.conn = conn
	a.log.Infof("Receiving AMQP 1.0 messages from source: %v\n", a.sourceAddr)
	return nil
}

func (a *amqp1Reader) disconnect(ctx context.Context) error {
	a.m.Lock()
	defer a.m.Unlock()

	if a.conn != nil {
		a.conn.Close(ctx)
	}
	a.conn = nil
	return nil
}

func (a *amqp1Reader) ReadBatch(ctx context.Context) (service.MessageBatch, service.AckFunc, error) {
	a.m.RLock()
	conn := a.conn
	a.m.RUnlock()

	if conn == nil {
		return nil, nil, service.ErrNotConnected
	}

	// Receive next message
	amqpMsg, err := conn.receiver.Receive(ctx)
	if err != nil {
		if err == amqp.ErrTimeout || ctx.Err() != nil {
			err = component.ErrTimeout
		} else {
			if dErr, isDetachError := err.(*amqp.DetachError); isDetachError && dErr.RemoteError != nil {
				a.log.Errorf("Lost connection due to: %v", dErr.RemoteError)
			} else {
				a.log.Errorf("Lost connection due to: %v", err)
			}
			_ = a.disconnect(ctx)
			err = service.ErrNotConnected
		}
		return nil, nil, err
	}

	var part *service.Message

	if data := amqpMsg.GetData(); data != nil {
		part = service.NewMessage(data)
	} else if value, ok := amqpMsg.Value.(string); ok {
		part = service.NewMessage([]byte(value))
	} else {
		part = service.NewMessage(nil)
	}

	if amqpMsg.Properties != nil {
		amqpSetMetadata(part, "amqp_content_type", amqpMsg.Properties.ContentType)
		amqpSetMetadata(part, "amqp_content_encoding", amqpMsg.Properties.ContentEncoding)
		amqpSetMetadata(part, "amqp_creation_time", amqpMsg.Properties.CreationTime)
	}
	if amqpMsg.Annotations != nil {
		for k, v := range amqpMsg.Annotations {
			keyStr, keyIsStr := k.(string)
			valStr, valIsStr := v.(string)
			if keyIsStr && valIsStr {
				amqpSetMetadata(part, keyStr, valStr)
			}
		}
	}

	var done chan struct{}
	if a.renewLock {
		done = a.startRenewJob(amqpMsg)
	}

	return service.MessageBatch{part}, func(ctx context.Context, res error) error {
		if done != nil {
			close(done)
			done = nil
		}

		// TODO: These methods were moved in v0.16.0, but nacking seems broken
		// (integration tests fail)
		if res != nil {
			return conn.receiver.ModifyMessage(ctx, amqpMsg, true, false, amqpMsg.Annotations)
		}
		return conn.receiver.AcceptMessage(ctx, amqpMsg)
	}, nil
}

func (a *amqp1Reader) Close(ctx context.Context) error {
	return a.disconnect(ctx)
}

//------------------------------------------------------------------------------

type amqp1Conn struct {
	client            *amqp.Client
	session           *amqp.Session
	receiver          *amqp.Receiver
	renewLockReceiver *amqp.Receiver
	renewLockSender   *amqp.Sender

	log                    *service.Logger
	lockRenewAddressPrefix string
}

func (c *amqp1Conn) Close(ctx context.Context) error {
	if c.renewLockSender != nil {
		if err := c.renewLockSender.Close(ctx); err != nil {
			c.log.Errorf("Failed to cleanly close renew lock sender: %v\n", err)
		}
	}
	if c.renewLockReceiver != nil {
		if err := c.renewLockReceiver.Close(ctx); err != nil {
			c.log.Errorf("Failed to cleanly close renew lock receiver: %v\n", err)
		}
	}
	if c.receiver != nil {
		if err := c.receiver.Close(ctx); err != nil {
			c.log.Errorf("Failed to cleanly close receiver: %v\n", err)
		}
	}
	if c.session != nil {
		if err := c.session.Close(ctx); err != nil {
			c.log.Errorf("Failed to cleanly close session: %v\n", err)
		}
	}
	if c.client != nil {
		if err := c.client.Close(); err != nil {
			c.log.Errorf("Failed to cleanly close client: %v\n", err)
		}
	}
	return nil
}

const (
	lockRenewResponseSuffix = "-response"
	lockRenewRequestSuffix  = "-request"
)

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

var seededRand = rand.New(rand.NewSource(time.Now().UnixNano()))

func randomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[seededRand.Intn(len(letterBytes))]
	}
	return string(b)
}

func (a *amqp1Reader) startRenewJob(amqpMsg *amqp.Message) chan struct{} {
	done := make(chan struct{})
	go func() {
		ctx := context.Background()

		lockedUntil, ok := amqpMsg.Annotations["x-opt-locked-until"].(time.Time)
		if !ok {
			a.log.Error("Missing x-opt-locked-until annotation in received message")
			return
		}

		for {
			select {
			case <-done:
				return
			case <-time.After(time.Until(lockedUntil) / 10 * 9):
				var err error
				lockedUntil, err = a.renewWithContext(ctx, amqpMsg)
				if err != nil {
					a.log.Errorf("Unable to renew lock err: %v", err)
					return
				}

				a.log.Tracef("Renewed lock until %v", lockedUntil)
			}
		}
	}()
	return done
}

func uuidFromLockTokenBytes(bytes []byte) (*amqp.UUID, error) {
	if len(bytes) != 16 {
		return nil, fmt.Errorf("invalid lock token, token was not 16 bytes long")
	}

	swapIndex := func(indexOne, indexTwo int, array *[16]byte) {
		array[indexOne], array[indexTwo] = array[indexTwo], array[indexOne]
	}

	// Get lock token from the deliveryTag
	var lockTokenBytes [16]byte
	copy(lockTokenBytes[:], bytes[:16])
	// translate from .net guid byte serialisation format to amqp rfc standard
	swapIndex(0, 3, &lockTokenBytes)
	swapIndex(1, 2, &lockTokenBytes)
	swapIndex(4, 5, &lockTokenBytes)
	swapIndex(6, 7, &lockTokenBytes)
	amqpUUID := amqp.UUID(lockTokenBytes)

	return &amqpUUID, nil
}

func (a *amqp1Reader) renewWithContext(ctx context.Context, msg *amqp.Message) (time.Time, error) {
	a.m.RLock()
	conn := a.conn
	a.m.RUnlock()

	if conn == nil {
		return time.Time{}, component.ErrNotConnected
	}

	lockToken, err := uuidFromLockTokenBytes(msg.DeliveryTag)
	if err != nil {
		return time.Time{}, err
	}

	replyTo := conn.lockRenewAddressPrefix + lockRenewResponseSuffix
	renewMsg := &amqp.Message{
		Properties: &amqp.MessageProperties{
			MessageID: msg.Properties.MessageID,
			ReplyTo:   &replyTo,
		},
		ApplicationProperties: map[string]any{
			"operation": "com.microsoft:renew-lock",
		},
		Value: map[string]any{
			"lock-tokens": []amqp.UUID{*lockToken},
		},
	}

	err = conn.renewLockSender.Send(ctx, renewMsg)
	if err != nil {
		return time.Time{}, err
	}

	result, err := conn.renewLockReceiver.Receive(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if statusCode, ok := result.ApplicationProperties["statusCode"].(int32); !ok || statusCode != 200 {
		return time.Time{}, fmt.Errorf("unsuccessful status code %d, message %s", statusCode, result.ApplicationProperties["statusDescription"])
	}

	values, ok := result.Value.(map[string]any)
	if !ok {
		return time.Time{}, errors.New("missing value in response message")
	}

	expirations, ok := values["expirations"].([]time.Time)
	if !ok || len(expirations) != 1 {
		return time.Time{}, errors.New("missing expirations filed in response message values")
	}

	return expirations[0], nil
}

func amqpSetMetadata(p *service.Message, k string, v any) {
	var metaValue string
	metaKey := strings.ReplaceAll(k, "-", "_")

	switch v := v.(type) {
	case bool:
		metaValue = strconv.FormatBool(v)
	case float32:
		metaValue = strconv.FormatFloat(float64(v), 'f', -1, 32)
	case float64:
		metaValue = strconv.FormatFloat(v, 'f', -1, 64)
	case byte:
		metaValue = strconv.Itoa(int(v))
	case int16:
		metaValue = strconv.Itoa(int(v))
	case int32:
		metaValue = strconv.Itoa(int(v))
	case int64:
		metaValue = strconv.Itoa(int(v))
	case nil:
		metaValue = ""
	case string:
		metaValue = v
	case []byte:
		metaValue = string(v)
	case time.Time:
		metaValue = v.Format(time.RFC3339)
	default:
		metaValue = ""
	}

	if metaValue != "" {
		p.MetaSetMut(metaKey, metaValue)
	}
}
