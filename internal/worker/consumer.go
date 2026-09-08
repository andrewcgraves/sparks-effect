package worker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
)

const traceIDHeader = "x-trace-id"

const retryHeader = "x-routing-attempts"

const deadLetterReason = "x-routing-dead-letter-reason"

type Consumer struct {
	url        string
	queue      string
	deadLetter string
	maxRetries int
	handler    *Handler
	log        *logger.Logger
	mu         sync.Mutex
	conn       *amqp.Connection
	ch         *amqp.Channel
}

func NewConsumer(url, queue, deadLetter string, maxRetries int, handler *Handler, log *logger.Logger) *Consumer {
	return &Consumer{
		url:        url,
		queue:      queue,
		deadLetter: deadLetter,
		maxRetries: maxRetries,
		handler:    handler,
		log:        log,
	}
}

func (c *Consumer) Run(ctx context.Context) error {
	deliveries, err := c.connect()
	if err != nil {
		return err
	}
	defer c.Close()

	closed := c.ch.NotifyClose(make(chan *amqp.Error, 1))
	c.log.Info(ctx, "routing: consuming",
		"queue", c.queue, "dead_letter_queue", c.deadLetter, "max_retries", c.maxRetries)

	for {
		select {
		case <-ctx.Done():
			c.log.Info(ctx, "routing: shutting down")
			return nil

		case amqpErr := <-closed:
			if amqpErr != nil {
				return fmt.Errorf("routing: broker connection closed: %w", amqpErr)
			}
			return nil

		case d, ok := <-deliveries:
			if !ok {
				return errors.New("routing: delivery channel closed")
			}
			c.handle(ctx, d)
		}
	}
}

func (c *Consumer) handle(ctx context.Context, d amqp.Delivery) {
	ctx = logger.WithTraceID(ctx, traceIDFromDelivery(d))

	res := c.handler.Handle(ctx, d.Body)

	ctx = logger.WithTraceID(ctx, res.TraceID)

	switch res.Outcome {
	case OutcomeSucceeded, OutcomePermanent:

		c.ack(ctx, d, res)

	case OutcomeTransient:
		attempts := attemptCount(d.Headers) + 1
		if attempts > c.maxRetries {
			c.log.Warn(ctx, "routing: job exhausted retries; dead-lettering",
				"job_id", res.JobID, "max_retries", c.maxRetries)

			if res.JobID != "" {
				c.handler.Fail(ctx, res.JobID, fmt.Errorf("exhausted %d retries: %w", c.maxRetries, res.Err))
			}
			c.deadLetterDelivery(ctx, d, res)
			return
		}

		c.log.Warn(ctx, "routing: job attempt failed; requeueing",
			"job_id", res.JobID, "attempt", attempts, "max_retries", c.maxRetries, "err", errString(res.Err))
		c.requeue(ctx, d, attempts)
	}
}

func traceIDFromDelivery(d amqp.Delivery) string {
	v, ok := d.Headers[traceIDHeader]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (c *Consumer) requeue(ctx context.Context, d amqp.Delivery, attempts int) {
	headers := cloneHeaders(d.Headers)
	headers[retryHeader] = int32(attempts)

	if err := c.publish(c.queue, d, headers); err != nil {
		c.log.Error(ctx, "routing: republishing for retry failed", "err", err.Error())

		if nackErr := d.Nack(false, true); nackErr != nil {
			c.log.Error(ctx, "routing: nack failed", "err", nackErr.Error())
		}
		return
	}
	if err := d.Ack(false); err != nil {
		c.log.Error(ctx, "routing: ack after requeue failed", "err", err.Error())
	}
}

func (c *Consumer) deadLetterDelivery(ctx context.Context, d amqp.Delivery, res Result) {
	headers := cloneHeaders(d.Headers)
	if res.Err != nil {
		headers[deadLetterReason] = res.Err.Error()
	}

	if err := c.publish(c.deadLetter, d, headers); err != nil {
		c.log.Error(ctx, "routing: publishing to dead-letter queue failed", "err", err.Error())
	}
	if err := d.Ack(false); err != nil {
		c.log.Error(ctx, "routing: ack after dead-lettering failed", "err", err.Error())
	}
}

func (c *Consumer) ack(ctx context.Context, d amqp.Delivery, res Result) {
	if err := d.Ack(false); err != nil {
		c.log.Error(ctx, "routing: ack failed", "job_id", res.JobID, "err", err.Error())
	}
}

func (c *Consumer) publish(queue string, d amqp.Delivery, headers amqp.Table) error {
	c.mu.Lock()
	ch := c.ch
	c.mu.Unlock()
	if ch == nil {
		return errors.New("routing: no channel")
	}

	return ch.PublishWithContext(context.Background(), "", queue, false, false, amqp.Publishing{
		ContentType:  d.ContentType,
		DeliveryMode: amqp.Persistent,
		MessageId:    d.MessageId,
		Headers:      headers,
		Body:         d.Body,
	})
}

func (c *Consumer) connect() (<-chan amqp.Delivery, error) {
	conn, err := amqp.Dial(c.url)
	if err != nil {
		return nil, fmt.Errorf("routing: dial broker: %w", redactDialErr(c.url, err))
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("routing: open channel: %w", err)
	}

	if _, err := ch.QueueDeclare(c.queue, true, false, false, false, nil); err != nil {
		return nil, c.closeOnErr(conn, ch, fmt.Errorf("routing: declare queue %q: %w", c.queue, err))
	}
	if _, err := ch.QueueDeclare(c.deadLetter, true, false, false, false, nil); err != nil {
		return nil, c.closeOnErr(conn, ch, fmt.Errorf("routing: declare dead-letter queue %q: %w", c.deadLetter, err))
	}

	if err := ch.Qos(1, 0, false); err != nil {
		return nil, c.closeOnErr(conn, ch, fmt.Errorf("routing: set prefetch: %w", err))
	}

	deliveries, err := ch.Consume(c.queue, "", false, false, false, false, nil)
	if err != nil {
		return nil, c.closeOnErr(conn, ch, fmt.Errorf("routing: consume from %q: %w", c.queue, err))
	}

	c.mu.Lock()
	c.conn, c.ch = conn, ch
	c.mu.Unlock()
	return deliveries, nil
}

func redactDialErr(rawURL string, err error) error {
	cred := userinfo(rawURL)
	if cred == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), cred, "***"))
}

func userinfo(rawURL string) string {
	schemeEnd := strings.Index(rawURL, "://")
	if schemeEnd == -1 {
		return ""
	}
	authority := rawURL[schemeEnd+3:]
	if slash := strings.IndexByte(authority, '/'); slash != -1 {
		authority = authority[:slash]
	}
	at := strings.LastIndexByte(authority, '@')
	if at == -1 {
		return ""
	}
	return authority[:at]
}

func (c *Consumer) closeOnErr(conn *amqp.Connection, ch *amqp.Channel, err error) error {
	_ = ch.Close()
	_ = conn.Close()
	return err
}

func (c *Consumer) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch != nil {
		_ = c.ch.Close()
		c.ch = nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

func attemptCount(headers amqp.Table) int {
	v, ok := headers[retryHeader]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int8:
		return int(n)
	case int16:
		return int(n)
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if parsed, err := strconv.Atoi(n); err == nil {
			return parsed
		}
	}
	return 0
}

func cloneHeaders(h amqp.Table) amqp.Table {
	out := amqp.Table{}
	for k, v := range h {
		out[k] = v
	}
	return out
}
