package testsupport

import (
	"context"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Broker connects to the throwaway RabbitMQ named by TEST_AMQP_URL and returns
// a channel on it, along with the URL for anything that needs its own
// connection.
//
// A broker is not faked here for the same reason sparks-effect-api does not
// fake it on the publishing side: acknowledgement, redelivery and prefetch are
// properties of the broker conversation, and a fake that implemented them would
// be asserting its own behaviour rather than RabbitMQ's.
func Broker(t *testing.T) (url string, ch *amqp.Channel) {
	t.Helper()

	url = os.Getenv("TEST_AMQP_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_AMQP_URL is unset under CI; the integration suite must not silently skip")
		}
		t.Skip("TEST_AMQP_URL not set; skipping broker integration test")
	}

	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("dialling broker: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ch, err = conn.Channel()
	if err != nil {
		t.Fatalf("opening channel: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	return url, ch
}

// ResetQueues deletes and redeclares the given queues, so a test starts against
// an empty broker whatever an earlier one left behind.
func ResetQueues(t *testing.T, ch *amqp.Channel, queues ...string) {
	t.Helper()
	for _, q := range queues {
		if _, err := ch.QueueDelete(q, false, false, false); err != nil {
			t.Fatalf("deleting queue %q: %v", q, err)
		}
		if _, err := ch.QueueDeclare(q, true, false, false, false, nil); err != nil {
			t.Fatalf("declaring queue %q: %v", q, err)
		}
	}
}

// Publish puts a message on a queue via the default exchange, the way
// sparks-effect-api's publisher does.
func Publish(t *testing.T, ch *amqp.Channel, queue string, body []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := ch.PublishWithContext(ctx, "", queue, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Body:         body,
	}); err != nil {
		t.Fatalf("publishing to %q: %v", queue, err)
	}
}

// QueueDepth reports how many messages are sitting on a queue.
//
// Read via a passive declare, which returns the queue's current message count
// without modifying it. Messages that are delivered but not yet acknowledged do
// not count, which is exactly the property the retry tests want to observe.
func QueueDepth(t *testing.T, ch *amqp.Channel, queue string) int {
	t.Helper()
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		t.Fatalf("inspecting queue %q: %v", queue, err)
	}
	return q.Messages
}

// WaitFor polls cond until it holds or the deadline passes.
//
// Polling rather than waiting on a signal because the things being waited for
// — a row reaching a status, a message landing on a queue — happen in another
// process or another goroutine, with no channel to hand. The timeout is what
// turns "this never happened" into a failure rather than a hung test.
func WaitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}
