package redismq

import (
	"fmt"
	"time"

	"gopkg.in/redis.v2"
)

// Consumer are used for reading from queues
type Consumer struct {
	Name  string
	Queue *Queue
}

type readTimeout bool

func (readTimeout) Error() string {
	return "Timeout occurred reading from the consumer"
}

func (t readTimeout) Timeout() bool {
	return bool(t)
}

// AddConsumer returns a conumser that can write from the queue
func (queue *Queue) AddConsumer(name string) (c *Consumer, err error) {
	c = &Consumer{Name: name, Queue: queue}
	//check uniqueness and start heartbeat
	added, err := queue.redisClient.SAdd(queueWorkersKey(queue.Name), name).Result()
	if err != nil {
		return nil, err
	}
	if added == 0 {
		if queue.isActiveConsumer(name) {
			return nil, fmt.Errorf("consumer with this name is already active")
		}
	}
	c.startHeartbeat()
	return c, nil
}

// Get returns a single package from the queue (blocking)
func (consumer *Consumer) Get(timeout time.Duration) (*Package, error) {
	if consumer.HasUnacked() {
		return nil, fmt.Errorf("unacked Packages found")
	}
	pkg, err := consumer.unsafeGet(timeout)
	if err == redis.Nil {
		return pkg, readTimeout(true)
	} else {
		return pkg, err
	}
}

// NoWaitGet returns a single package from the queue (returns nil, nil if no package in queue)
func (consumer *Consumer) NoWaitGet() (*Package, error) {
	if consumer.HasUnacked() {
		return nil, fmt.Errorf("unacked Packages found")
	}
	answer := consumer.Queue.redisClient.RPopLPush(
		queueInputKey(consumer.Queue.Name),
		consumerWorkingQueueKey(consumer.Queue.Name, consumer.Name),
	)
	if answer.Val() == "" {
		return nil, nil
	}
	consumer.Queue.incrRate(
		consumerWorkingRateKey(consumer.Queue.Name, consumer.Name),
		1,
	)
	return consumer.parseRedisAnswer(answer)
}

// MultiGet returns an array of packages from the queue
func (consumer *Consumer) MultiGet(length int, timeout time.Duration) ([]*Package, error) {
	var collection []*Package
	if consumer.HasUnacked() {
		return nil, fmt.Errorf("unacked Packages found")
	}

	// TODO maybe use transactions for rollback in case of errors?
	reqs, err := consumer.Queue.redisClient.Pipelined(func(c *redis.Pipeline) error {
		c.BRPopLPush(
			queueInputKey(consumer.Queue.Name),
			consumerWorkingQueueKey(consumer.Queue.Name, consumer.Name),
			int64(timeout/time.Second),
		)
		for i := 1; i < length; i++ {
			c.RPopLPush(
				queueInputKey(consumer.Queue.Name),
				consumerWorkingQueueKey(consumer.Queue.Name, consumer.Name),
			)
		}
		return nil
	})
	if err != nil && err != redis.Nil {
		return nil, err
	}

	for _, answer := range reqs {
		switch answer := answer.(type) {
		case *redis.StringCmd:
			if answer.Val() == "" {
				continue
			}
			p, err := consumer.parseRedisAnswer(answer)
			if err != nil {
				return nil, err
			}
			p.Collection = &collection
			collection = append(collection, p)
		default:
			return nil, err
		}
	}
	consumer.Queue.incrRate(
		consumerWorkingRateKey(consumer.Queue.Name, consumer.Name),
		int64(length),
	)

	return collection, nil
}

// GetUnacked returns a single packages from the working queue of this consumer
func (consumer *Consumer) GetUnacked() (*Package, error) {
	if !consumer.HasUnacked() {
		return nil, fmt.Errorf("no unacked Packages found")
	}
	answer := consumer.Queue.redisClient.LIndex(
		consumerWorkingQueueKey(consumer.Queue.Name, consumer.Name),
		-1,
	)
	return consumer.parseRedisAnswer(answer)
}

// HasUnacked returns true if the consumers has unacked packages
func (consumer *Consumer) HasUnacked() bool {
	if consumer.GetUnackedLength() != 0 {
		return true
	}
	return false
}

// GetUnackedLength returns the number of packages in the unacked queue
func (consumer *Consumer) GetUnackedLength() int64 {
	return consumer.Queue.redisClient.LLen(consumerWorkingQueueKey(consumer.Queue.Name, consumer.Name)).Val()
}

// GetFailed returns a single packages from the failed queue of this consumer
func (consumer *Consumer) GetFailed() (*Package, error) {
	answer := consumer.Queue.redisClient.RPopLPush(
		queueFailedKey(consumer.Queue.Name),
		consumerWorkingQueueKey(consumer.Queue.Name, consumer.Name),
	)
	consumer.Queue.incrRate(
		consumerWorkingRateKey(consumer.Queue.Name, consumer.Name),
		1,
	)
	return consumer.parseRedisAnswer(answer)
}

// ResetWorking deletes! all messages in the working queue of this consumer
func (consumer *Consumer) ResetWorking() error {
	return consumer.Queue.redisClient.Del(consumerWorkingQueueKey(consumer.Queue.Name, consumer.Name)).Err()
}

// RequeueWorking requeues all packages from working to input
func (consumer *Consumer) RequeueWorking() error {
	for consumer.HasUnacked() {
		p, err := consumer.GetUnacked()
		if err != nil {
			return err
		}
		p.Requeue()
	}
	return nil
}

func (consumer *Consumer) ackPackage(p *Package) error {
	return consumer.Queue.redisClient.RPop(consumerWorkingQueueKey(consumer.Queue.Name, consumer.Name)).Err()
}

func (consumer *Consumer) requeuePackage(p *Package) error {
	answer := consumer.Queue.redisClient.RPopLPush(
		consumerWorkingQueueKey(consumer.Queue.Name, consumer.Name),
		queueInputKey(consumer.Queue.Name),
	)
	consumer.Queue.incrRate(queueInputRateKey(consumer.Queue.Name), 1)
	return answer.Err()
}

func (consumer *Consumer) failPackage(p *Package) error {
	return consumer.Queue.redisClient.RPopLPush(
		consumerWorkingQueueKey(consumer.Queue.Name, consumer.Name),
		queueFailedKey(consumer.Queue.Name),
	).Err()
}

func (consumer *Consumer) startHeartbeat() {
	firstWrite := make(chan bool, 1)
	go func() {
		firstRun := true
		for {
			consumer.Queue.redisClient.SetEx(
				consumerHeartbeatKey(consumer.Queue.Name, consumer.Name),
				time.Second,
				"ping",
			)
			if firstRun {
				firstWrite <- true
				firstRun = false
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	<-firstWrite
	return
}

func (consumer *Consumer) parseRedisAnswer(answer *redis.StringCmd) (*Package, error) {
	if answer.Err() != nil {
		return nil, answer.Err()
	}
	p, err := unmarshalPackage(answer.Val(), consumer.Queue, consumer)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (consumer *Consumer) unsafeGet(timeout time.Duration) (*Package, error) {
	answer := consumer.Queue.redisClient.BRPopLPush(
		queueInputKey(consumer.Queue.Name),
		consumerWorkingQueueKey(consumer.Queue.Name, consumer.Name),
		int64(timeout/time.Second),
	)
	consumer.Queue.incrRate(
		consumerWorkingRateKey(consumer.Queue.Name, consumer.Name),
		1,
	)
	return consumer.parseRedisAnswer(answer)
}
