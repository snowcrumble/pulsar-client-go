// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package pulsar

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/apache/pulsar-client-go/pulsar/internal"
	pb "github.com/apache/pulsar-client-go/pulsar/internal/pulsar_proto"
)

var ErrConsumerClosed = errors.New("consumer closed")

const defaultNackRedeliveryDelay = 1 * time.Minute

type acker interface {
	AckID(id *messageID)
	NackID(id *messageID)
}

type consumer struct {
	sync.Mutex
	topic                     string
	client                    *client
	options                   ConsumerOptions
	consumers                 []*partitionConsumer
	consumerName              string
	disableForceTopicCreation bool

	// channel used to deliver message to clients
	messageCh chan ConsumerMessage

	dlq       *dlqRouter
	closeOnce sync.Once
	closeCh   chan struct{}
	errorCh   chan error
	ticker    *time.Ticker

	log *log.Entry
}

func newConsumer(client *client, options ConsumerOptions) (Consumer, error) {
	if options.Topic == "" && options.Topics == nil && options.TopicsPattern == "" {
		return nil, newError(TopicNotFound, "topic is required")
	}

	if options.SubscriptionName == "" {
		return nil, newError(SubscriptionNotFound, "subscription name is required for consumer")
	}

	if options.ReceiverQueueSize <= 0 {
		options.ReceiverQueueSize = 1000
	}

	// did the user pass in a message channel?
	messageCh := options.MessageChannel
	if options.MessageChannel == nil {
		messageCh = make(chan ConsumerMessage, 10)
	}

	dlq, err := newDlqRouter(client, options.DLQ)
	if err != nil {
		return nil, err
	}

	// single topic consumer
	if options.Topic != "" || len(options.Topics) == 1 {
		topic := options.Topic
		if topic == "" {
			topic = options.Topics[0]
		}

		if err := validateTopicNames(topic); err != nil {
			return nil, err
		}

		return topicSubscribe(client, options, topic, messageCh, dlq)
	}

	if len(options.Topics) > 1 {
		if err := validateTopicNames(options.Topics...); err != nil {
			return nil, err
		}

		return newMultiTopicConsumer(client, options, options.Topics, messageCh, dlq)
	}

	if options.TopicsPattern != "" {
		tn, err := internal.ParseTopicName(options.TopicsPattern)
		if err != nil {
			return nil, err
		}

		pattern, err := extractTopicPattern(tn)
		if err != nil {
			return nil, err
		}
		return newRegexConsumer(client, options, tn, pattern, messageCh, dlq)
	}

	return nil, newError(ResultInvalidTopicName, "topic name is required for consumer")
}

func newInternalConsumer(client *client, options ConsumerOptions, topic string,
	messageCh chan ConsumerMessage, dlq *dlqRouter, disableForceTopicCreation bool) (*consumer, error) {

	consumer := &consumer{
		topic:                     topic,
		client:                    client,
		options:                   options,
		disableForceTopicCreation: disableForceTopicCreation,
		messageCh:                 messageCh,
		closeCh:                   make(chan struct{}),
		errorCh:                   make(chan error),
		dlq:                       dlq,
		log:                       log.WithField("topic", topic),
	}

	if options.Name != "" {
		consumer.consumerName = options.Name
	} else {
		consumer.consumerName = generateRandomName()
	}

	err := consumer.internalTopicSubscribeToPartitions()
	if err != nil {
		return nil, err
	}

	// set up timer to monitor for new partitions being added
	duration := options.AutoDiscoveryPeriod
	if duration <= 0 {
		duration = defaultAutoDiscoveryDuration
	}
	consumer.ticker = time.NewTicker(duration)

	go func() {
		for range consumer.ticker.C {
			consumer.log.Debug("Auto discovering new partitions")
			consumer.internalTopicSubscribeToPartitions()
		}
	}()

	return consumer, nil
}

func (c *consumer) internalTopicSubscribeToPartitions() error {
	partitions, err := c.client.TopicPartitions(c.topic)
	if err != nil {
		return err
	}

	oldNumPartitions := 0
	newNumPartitions := len(partitions)

	c.Lock()
	defer c.Unlock()
	oldConsumers := c.consumers

	if oldConsumers != nil {
		oldNumPartitions = len(oldConsumers)
		if oldNumPartitions == newNumPartitions {
			c.log.Debug("Number of partitions in topic has not changed")
			return nil
		}

		c.log.WithField("old_partitions", oldNumPartitions).
			WithField("new_partitions", newNumPartitions).
			Info("Changed number of partitions in topic")
	}

	c.consumers = make([]*partitionConsumer, newNumPartitions)

	// Copy over the existing consumer instances
	for i := 0; i < oldNumPartitions; i++ {
		c.consumers[i] = oldConsumers[i]
	}

	type ConsumerError struct {
		err       error
		partition int
		consumer  *partitionConsumer
	}

	receiverQueueSize := c.options.ReceiverQueueSize
	metadata := c.options.Properties

	partitionsToAdd := newNumPartitions - oldNumPartitions
	var wg sync.WaitGroup
	ch := make(chan ConsumerError, partitionsToAdd)
	wg.Add(partitionsToAdd)

	for partitionIdx := oldNumPartitions; partitionIdx < newNumPartitions; partitionIdx++ {
		partitionTopic := partitions[partitionIdx]

		go func(idx int, pt string) {
			defer wg.Done()

			var nackRedeliveryDelay time.Duration
			if c.options.NackRedeliveryDelay == 0 {
				nackRedeliveryDelay = defaultNackRedeliveryDelay
			} else {
				nackRedeliveryDelay = c.options.NackRedeliveryDelay
			}
			opts := &partitionConsumerOpts{
				topic:                      pt,
				consumerName:               c.consumerName,
				subscription:               c.options.SubscriptionName,
				subscriptionType:           c.options.Type,
				subscriptionInitPos:        c.options.SubscriptionInitialPosition,
				partitionIdx:               idx,
				receiverQueueSize:          receiverQueueSize,
				nackRedeliveryDelay:        nackRedeliveryDelay,
				metadata:                   metadata,
				replicateSubscriptionState: c.options.ReplicateSubscriptionState,
				startMessageID:             nil,
				subscriptionMode:           durable,
				readCompacted:              c.options.ReadCompacted,
			}
			cons, err := newPartitionConsumer(c, c.client, opts, c.messageCh, c.dlq)
			ch <- ConsumerError{
				err:       err,
				partition: idx,
				consumer:  cons,
			}
		}(partitionIdx, partitionTopic)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	for ce := range ch {
		if ce.err != nil {
			err = ce.err
		} else {
			c.consumers[ce.partition] = ce.consumer
		}
	}

	if err != nil {
		// Since there were some failures,
		// cleanup all the partitions that succeeded in creating the consumer
		for _, c := range c.consumers {
			if c != nil {
				c.Close()
			}
		}
		return err
	}

	return nil
}

func topicSubscribe(client *client, options ConsumerOptions, topic string,
	messageCh chan ConsumerMessage, dlqRouter *dlqRouter) (Consumer, error) {
	return newInternalConsumer(client, options, topic, messageCh, dlqRouter, false)
}

func (c *consumer) Subscription() string {
	return c.options.SubscriptionName
}

func (c *consumer) Unsubscribe() error {
	c.Lock()
	defer c.Unlock()

	var errMsg string
	for _, consumer := range c.consumers {
		if err := consumer.Unsubscribe(); err != nil {
			errMsg += fmt.Sprintf("topic %s, subscription %s: %s", consumer.topic, c.Subscription(), err)
		}
	}
	if errMsg != "" {
		return fmt.Errorf(errMsg)
	}
	return nil
}

func (c *consumer) Receive(ctx context.Context) (message Message, err error) {
	for {
		select {
		case <-c.closeCh:
			return nil, ErrConsumerClosed
		case cm, ok := <-c.messageCh:
			if !ok {
				return nil, ErrConsumerClosed
			}
			return cm.Message, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Messages
func (c *consumer) Chan() <-chan ConsumerMessage {
	return c.messageCh
}

// Ack the consumption of a single message
func (c *consumer) Ack(msg Message) {
	c.AckID(msg.ID())
}

// Ack the consumption of a single message, identified by its MessageID
func (c *consumer) AckID(msgID MessageID) {
	mid, ok := c.messageID(msgID)
	if !ok {
		return
	}

	if mid.consumer != nil {
		mid.Ack()
		return
	}

	c.consumers[mid.partitionIdx].AckID(mid)
}

func (c *consumer) Nack(msg Message) {
	c.NackID(msg.ID())
}

func (c *consumer) NackID(msgID MessageID) {
	mid, ok := c.messageID(msgID)
	if !ok {
		return
	}

	if mid.consumer != nil {
		mid.Nack()
		return
	}

	c.consumers[mid.partitionIdx].NackID(mid)
}

func (c *consumer) Close() {
	c.closeOnce.Do(func() {
		c.Lock()
		defer c.Unlock()

		var wg sync.WaitGroup
		for i := range c.consumers {
			wg.Add(1)
			go func(pc *partitionConsumer) {
				defer wg.Done()
				pc.Close()
			}(c.consumers[i])
		}
		wg.Wait()
		close(c.closeCh)
		c.ticker.Stop()
		c.client.handlers.Del(c)
		c.dlq.close()
	})
}

func (c *consumer) Seek(msgID MessageID) error {
	c.Lock()
	defer c.Unlock()

	if len(c.consumers) > 1 {
		return errors.New("for partition topic, seek command should perform on the individual partitions")
	}

	mid, ok := c.messageID(msgID)
	if !ok {
		return nil
	}

	return c.consumers[mid.partitionIdx].Seek(mid)
}

func (c *consumer) SeekByTime(time time.Time) error {
	c.Lock()
	defer c.Unlock()
	if len(c.consumers) > 1 {
		return errors.New("for partition topic, seek command should perform on the individual partitions")
	}

	return c.consumers[0].SeekByTime(time)
}

var r = &random{
	R: rand.New(rand.NewSource(time.Now().UnixNano())),
}

type random struct {
	sync.Mutex
	R *rand.Rand
}

func generateRandomName() string {
	r.Lock()
	defer r.Unlock()
	chars := "abcdefghijklmnopqrstuvwxyz"
	bytes := make([]byte, 5)
	for i := range bytes {
		bytes[i] = chars[r.R.Intn(len(chars))]
	}
	return string(bytes)
}

func toProtoSubType(st SubscriptionType) pb.CommandSubscribe_SubType {
	switch st {
	case Exclusive:
		return pb.CommandSubscribe_Exclusive
	case Shared:
		return pb.CommandSubscribe_Shared
	case Failover:
		return pb.CommandSubscribe_Failover
	case KeyShared:
		return pb.CommandSubscribe_Key_Shared
	}

	return pb.CommandSubscribe_Exclusive
}

func toProtoInitialPosition(p SubscriptionInitialPosition) pb.CommandSubscribe_InitialPosition {
	switch p {
	case SubscriptionPositionLatest:
		return pb.CommandSubscribe_Latest
	case SubscriptionPositionEarliest:
		return pb.CommandSubscribe_Earliest
	}

	return pb.CommandSubscribe_Latest
}

func (c *consumer) messageID(msgID MessageID) (*messageID, bool) {
	mid, ok := msgID.(*messageID)
	if !ok {
		c.log.Warnf("invalid message id type")
		return nil, false
	}

	partition := mid.partitionIdx
	// did we receive a valid partition index?
	if partition < 0 || partition >= len(c.consumers) {
		c.log.Warnf("invalid partition index %d expected a partition between [0-%d]",
			partition, len(c.consumers))
		return nil, false
	}

	return mid, true
}
