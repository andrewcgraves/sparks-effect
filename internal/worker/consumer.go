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

// traceIDHeader carries a trace id set by whoever published the delivery, so a
// job's whole log trail can be filtered on one value. It rides as a custom
// header rather than AMQP's standard CorrelationId property: MessageId /
// CorrelationId are the queue's own bookkeeping fields, easy for an
// intermediary to rewrite or drop, while a plain header round-trips untouched
// through requeue and dead-lettering alongside x-routing-attempts.
//
// It is not what carries the trace in production. sparks-effect-api puts the
// trace in the message body (routing.Message.TraceID) and sets no header at
// all, so this is only the trace available *before* the body is decoded, plus
// a way to trace a message injected by hand. Its absence is normal rather than
// an error; see traceIDFromDelivery, and handle for how the body's trace
// supersedes it.
const traceIDHeader = "x-trace-id"

// retryHeader counts how many times a delivery has been requeued by this
// worker.
//
// It is carried in the message rather than kept in memory because redelivery
// puts the message back on the broker, where an in-memory counter would not
// follow it: the next attempt could land on a different worker, or on this one
// after a restart, and either way the count would begin again at zero. AMQP's
// own redelivered flag is only a boolean, so it cannot bound anything.
const retryHeader = "x-routing-attempts"

// deadLetterReason records why a message ended up on the dead-letter queue, so
// whoever reads it later does not have to infer it from the job row.
const deadLetterReason = "x-routing-dead-letter-reason"

// Consumer runs the broker side of the loop: one message at a time, acked only
// once its result is safely written.
type Consumer struct {
	url        string
	queue      string
	deadLetter string
	maxRetries int
	handler    *Handler
	log        *logger.Logger

	// mu guards the connection, which Close may reach while Run is using it.
	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

// NewConsumer builds a consumer. It does not connect; Run does.
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

// Run consumes until ctx is cancelled or the broker connection drops.
//
// It returns rather than reconnecting internally. A worker whose broker has
// gone away has nothing to do, and letting the process exit hands the restart
// to Kubernetes, which already knows how to back off and how to report a pod
// that cannot stay up. A reconnect loop in here would hide exactly that.
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

// handle processes one delivery and settles it.
//
// The handler is given the *unconsumed* context rather than one bounded here:
// the acknowledgement has to happen even when the process is shutting down, and
// a chain cancelled half-way through settles as a transient failure and is
// redelivered, which is the intended behaviour for a worker killed mid-chain.
func (c *Consumer) handle(ctx context.Context, d amqp.Delivery) {
	ctx = logger.WithTraceID(ctx, traceIDFromDelivery(d))

	res := c.handler.Handle(ctx, d.Body)

	// The handler is the first thing to see the message body, and the trace the
	// API actually sets is in there rather than in a header (see traceIDHeader).
	// Adopting it here puts everything said about settling this delivery —
	// requeued, dead-lettered, failed for exhaustion — on the same trail as the
	// job itself. Empty for a message that carried no trace, which leaves the
	// header's tag alone.
	ctx = logger.WithTraceID(ctx, res.TraceID)

	switch res.Outcome {
	case OutcomeSucceeded, OutcomePermanent:
		// Ack only now — after the row is written. A crash before this point
		// redelivers the message rather than silently losing the work, which is
		// safe because every write is keyed by job id and recomputing over an
		// immutable graph gives the same answer.
		c.ack(ctx, d, res)

	case OutcomeTransient:
		attempts := attemptCount(d.Headers) + 1
		if attempts > c.maxRetries {
			c.log.Warn(ctx, "routing: job exhausted retries; dead-lettering",
				"job_id", res.JobID, "max_retries", c.maxRetries)
			// Mark failed *before* dead-lettering, so the moment the message
			// leaves the queue the row already says why. A client polling this
			// job stops waiting here; without it, it would wait forever on a
			// message nothing will consume again.
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

// traceIDFromDelivery reads the caller's trace id off a delivery's headers.
// An absent or non-string value is not an error — see traceIDHeader — and
// simply yields no trace id.
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

// errString reports err's message, or "" for a nil error — res.Err is nil on
// a transient outcome the handler itself did not attach a cause to.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// requeue publishes the delivery back onto its own queue with an incremented
// attempt count, then acks the original.
//
// Nack with requeue=true would be simpler and is not usable here: it puts the
// original bytes back unchanged, so the attempt counter would never advance and
// a failing job would loop forever. Republishing is what makes the bound real.
//
// The ack comes after the publish, so a crash between the two redelivers the
// original — one extra attempt, rather than a lost job.
func (c *Consumer) requeue(ctx context.Context, d amqp.Delivery, attempts int) {
	headers := cloneHeaders(d.Headers)
	headers[retryHeader] = int32(attempts)

	if err := c.publish(c.queue, d, headers); err != nil {
		c.log.Error(ctx, "routing: republishing for retry failed", "err", err.Error())
		// Nack with requeue so the message is not lost. The counter does not
		// advance, but the alternative is dropping a job outright.
		if nackErr := d.Nack(false, true); nackErr != nil {
			c.log.Error(ctx, "routing: nack failed", "err", nackErr.Error())
		}
		return
	}
	if err := d.Ack(false); err != nil {
		c.log.Error(ctx, "routing: ack after requeue failed", "err", err.Error())
	}
}

// deadLetterDelivery moves an exhausted message onto the dead-letter queue.
//
// A message that cannot be moved is acked anyway. The job row already says
// failed by this point, so the caller has its answer; keeping the message on
// the live queue would only mean retrying, past its bound, work that has
// already been reported as over.
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

// publish sends a delivery's body to a queue on the default exchange, which
// routes by exact queue name.
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

// connect dials the broker, declares both queues, and starts consuming.
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

	// Durable, non-exclusive, non-auto-delete — the same arguments
	// sparks-effect-api declares this queue with. A mismatch is rejected by the
	// broker rather than silently creating a second queue, so declaring it on
	// both sides is a check that the two agree, not a duplication.
	//
	// Classic queues, not quorum: the broker is a single node, so a quorum
	// queue has nobody to reach quorum with and costs memory for the privilege.
	if _, err := ch.QueueDeclare(c.queue, true, false, false, false, nil); err != nil {
		return nil, c.closeOnErr(conn, ch, fmt.Errorf("routing: declare queue %q: %w", c.queue, err))
	}
	if _, err := ch.QueueDeclare(c.deadLetter, true, false, false, false, nil); err != nil {
		return nil, c.closeOnErr(conn, ch, fmt.Errorf("routing: declare dead-letter queue %q: %w", c.deadLetter, err))
	}

	// Prefetch 1. A chain fans out to many concurrent routing calls against a
	// server with a handful of threads, so a second chain in flight does not
	// add throughput — it just makes both slower and holds a second job in an
	// unacknowledged state for longer.
	if err := ch.Qos(1, 0, false); err != nil {
		return nil, c.closeOnErr(conn, ch, fmt.Errorf("routing: set prefetch: %w", err))
	}

	// autoAck false: the whole redelivery guarantee rests on acking after the
	// row is written, and autoAck would ack on delivery instead.
	deliveries, err := ch.Consume(c.queue, "", false, false, false, false, nil)
	if err != nil {
		return nil, c.closeOnErr(conn, ch, fmt.Errorf("routing: consume from %q: %w", c.queue, err))
	}

	c.mu.Lock()
	c.conn, c.ch = conn, ch
	c.mu.Unlock()
	return deliveries, nil
}

// redactDialErr strips the AMQP URL's credentials out of a dial error.
//
// amqp091-go parses the URL with net/url, whose own parse errors embed the
// full input string — password included — in their Error() text. A password
// with an unescaped reserved character (a literal '%', '"', '^', ...) would
// otherwise put the plaintext credential in whatever log line reports the
// failure.
func redactDialErr(rawURL string, err error) error {
	cred := userinfo(rawURL)
	if cred == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), cred, "***"))
}

// userinfo extracts the "user:pass" portion of an amqp(s) URL by hand rather
// than with net/url, since a password net/url itself refuses to parse is
// exactly the case this exists to handle. The delimiter is the last '@'
// before the authority ends: the password may itself contain '@', but the
// host may not.
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

// Close releases the broker connection. Safe on a consumer that never
// connected, and safe to call twice.
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

// attemptCount reads the retry counter, tolerating every integer type the AMQP
// table may decode it as. A header written as int32 by this worker can come
// back as int64 or a string through a broker that rewrote it, and a counter
// misread as zero would make the retry bound unenforceable.
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
