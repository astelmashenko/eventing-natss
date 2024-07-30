/*
Copyright 2021 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"go.opencensus.io/trace"
	"io"
	"knative.dev/eventing-natss/pkg/tracing"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/cloudevents/sdk-go/v2/binding"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
	"knative.dev/eventing-natss/pkg/channel/jetstream/dispatcher/internal"
	eventingchannels "knative.dev/eventing/pkg/channel"
	"knative.dev/eventing/pkg/channel/fanout"
	"knative.dev/eventing/pkg/kncloudevents"
	"knative.dev/pkg/logging"
	//tracingnats "knative.dev/eventing-natss/pkg/tracing/nats"
)

const (
	batchGoRoutinesEnv = "BATCH_GOROUTINES"
	fetchBatchSizeEnv  = "FETCH_BATCH_SIZE"
)

var (
	// NumBatchGoRoutines is the number of goroutines that will be used to consume messages from
	// the queue, there are a number of factors you will want to tweak this number. This can be
	// configured via the BATCH_GOROUTINES environment variable.
	//
	// If you have sufficient resources, it is better to have NumBatchGoRoutines equal to
	// FetchBatchSize so that messages received by JetStream are immediately processed. This will
	// give the upstream request the closest amount of time to respond to the consumer's configured
	// AckWait. This is beneficial because if the eventing subscriber is a scale-to-zero service,
	// you have more time for resources to spin up.
	//
	// If you have limited resources, you may want to reduce the number of goroutines.
	NumBatchGoRoutines = 32

	// FetchBatchSize is the number of messages that will be fetched from JetStream in a single
	// request. This can be configured via the FETCH_BATCH_SIZE environment variable.
	//
	// If you expect to process a high-volume of messages, you may want to increase this number to
	// reduce the number of requests made to JetStream. Depending on the expected latency eventing
	// subscribers, you may also want to increase NumBatchGoRoutines.
	FetchBatchSize = 32
)

func init() {
	batchSize, err := strconv.Atoi(os.Getenv(batchGoRoutinesEnv))
	if err == nil {
		NumBatchGoRoutines = batchSize
	}

	fetchSize, err := strconv.Atoi(os.Getenv(fetchBatchSizeEnv))
	if err == nil {
		FetchBatchSize = fetchSize
	}
}

type PullingConsumer struct {
	dispatcher       *kncloudevents.Dispatcher
	reporter         eventingchannels.StatsReporter
	channelNamespace string

	natsConsumer     jetstream.Consumer
	natsConsumerInfo *jetstream.ConsumerInfo

	logger *zap.SugaredLogger

	sub   Subscription
	subMu sync.RWMutex

	lifecycleMu sync.Mutex
	started     bool
	closing     chan struct{}
	closed      chan struct{}
}

func NewPullingConsumer(
	ctx context.Context,
	consumer jetstream.Consumer,
	subscription Subscription,
	dispatcher *kncloudevents.Dispatcher,
	reporter eventingchannels.StatsReporter,
	channelNamespace string,
) (*PullingConsumer, error) {
	consumerInfo, err := consumer.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get consumer info: %w", err)
	}

	logger := logging.FromContext(ctx)

	return &PullingConsumer{
		dispatcher:       dispatcher,
		reporter:         reporter,
		channelNamespace: channelNamespace,

		natsConsumer:     consumer,
		natsConsumerInfo: consumerInfo,

		logger: logger,

		sub: subscription,

		closing: make(chan struct{}),
		closed:  make(chan struct{}),
	}, nil
}

// Start begins the consumer and handles messages until Close is called. This method is blocking and
// will return an error if the consumer fails prematurely. A nil error will be returned upon being
// stopped by the Close method.
func (c *PullingConsumer) Start() error {
	if err := c.checkStart(); err != nil {
		return err
	}

	defer close(c.closed)

	ctx := logging.WithLogger(context.Background(), c.logger)

	var wg sync.WaitGroup

	// wait for handlers to finish
	defer wg.Wait()

	// all messages are attached with this ctx, upon closing the consumer, we will cancel this ctx
	// so that all pending messages are cancelled. This should cause any pending requests be
	// cancelled (which also results in a nack) and the batch to be drained.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for {
		// TODO move 200 into subscription config
		batch, err := c.natsConsumer.Fetch(FetchBatchSize, jetstream.FetchMaxWait(200*time.Millisecond))
		if err != nil {
			return err
		}

		if err := c.consumeMessages(ctx, batch, &wg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}
	}
}

// consumeMessages consumes messages from the batch and forwards them to the queue. The enqueued
// message includes a context which will cancel after the AckWait duration of the consumer.
//
// This method returns once the MessageBatch has been consumed, or upon a call to Consumer.Close.
// Returning as a result of Consumer.Close results in an io.EOF error.
func (c *PullingConsumer) consumeMessages(ctx context.Context, batch jetstream.MessageBatch, wg *sync.WaitGroup) error {
	for {
		select {
		case natMsg, ok := <-batch.Messages():
			if !ok {
				return batch.Error()
			}

			wg.Add(1)

			go func() {
				defer wg.Done()

				ctx := logging.WithLogger(ctx, c.logger.With(zap.String("msg_id", natMsg.Headers().Get(nats.MsgIdHdr))))
				msg := internal.NewMessage(ctx, natMsg, c.natsConsumerInfo.Config.AckWait)

				if err := c.handleMessage(msg); err != nil {
					// handleMessage only errors if the message cannot be finished, any other error
					// is consumed by msg.Finish(err)
					logging.FromContext(ctx).Errorw("failed to finish message", zap.Error(err))
				}
			}()
		case <-c.closing:
			return io.EOF
		}
	}
}

func (c *PullingConsumer) UpdateSubscription(sub Subscription) {
	// wait for any pending messages to be processed with the old subscription
	c.subMu.Lock()
	defer c.subMu.Unlock()

	c.sub = sub
}

func (c *PullingConsumer) handleMessage(msg internal.Message) (err error) {
	// ensure that c.sub is not modified while we are handling a message
	c.subMu.RLock()
	defer c.subMu.RUnlock()

	ctx := msg.Context()

	logger := logging.FromContext(ctx)

	if logger.Desugar().Core().Enabled(zap.DebugLevel) {
		var debugKVs []interface{}
		if meta, err := msg.Metadata(); err == nil {
			debugKVs = append(debugKVs, zap.Any("msg_metadata", meta))
		}

		logger.Debugw("received message from JetStream consumer", debugKVs...)
	}

	if msg.ReadEncoding() == binding.EncodingUnknown {
		return errors.New("received a message with unknown encoding")
	}

	event := tracing.ConvertJsMsgToEvent(c.logger.Desugar(), msg.NatsMessage())
	additionalHeaders := tracing.ConvertEventToHttpHeader(event)

	sc, ok := tracing.ParseSpanContext(event)
	var span *trace.Span
	if !ok {
		c.logger.Warn("Cannot parse the spancontext, creating a new span")
		ctx, span = trace.StartSpan(ctx, jsmChannel+"-"+string(c.sub.UID))
	} else {
		ctx, span = trace.StartSpanWithRemoteParent(ctx, jsmChannel+"-"+string(c.sub.UID), sc)
	}

	defer span.End()

	te := TypeExtractorTransformer("")

	dispatchExecutionInfo, err := SendMessage(
		c.dispatcher,
		ctx,
		msg,
		c.sub.Subscriber,
		c.natsConsumerInfo.Config.AckWait,
		internal.NewJsMessageWrapper(msg.NatsMessage()),
		WithReply(c.sub.Reply),
		WithDeadLetterSink(c.sub.DeadLetter),
		WithRetryConfig(c.sub.RetryConfig),
		WithTransformers(&te),
		WithHeader(additionalHeaders),
	)

	_ = fanout.ParseDispatchResultAndReportMetrics(fanout.NewDispatchResult(err, dispatchExecutionInfo), c.reporter, eventingchannels.ReportArgs{
		Ns:        c.channelNamespace,
		EventType: string(te),
	})

	if err != nil {
		logger.Errorw("failed to forward message to downstream subscriber",
			zap.Error(err),
			zap.Any("dispatch_resp_code", dispatchExecutionInfo.ResponseCode))

		// let knative decide what to do with the message, if it wraps an Ack/Nack then that is what will happen,
		// otherwise we will Terminate the message
		return err
	}

	logger.Debugw("dispatched message to subscriber",
		zap.Int("response_code", dispatchExecutionInfo.ResponseCode))

	return nil
}

func (c *PullingConsumer) Close() error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	close(c.closing)

	<-c.closed

	// allow reusing the consumer - not sure if this is required but adds negligible overhead.
	c.started = false
	c.closing = make(chan struct{})
	c.closed = make(chan struct{})

	return nil
}

// checkStart ensures that the consumer is not already running and marks it as started.
func (c *PullingConsumer) checkStart() error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	if c.started {
		return errors.New("consumer already started")
	}

	c.started = true

	return nil
}
