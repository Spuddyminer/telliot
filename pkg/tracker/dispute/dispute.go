// Copyright (c) The Tellor Authors.
// Licensed under the MIT License.

package dispute

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/event"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/timestamp"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/tellor-io/telliot/pkg/contracts"
	"github.com/tellor-io/telliot/pkg/contracts/tellor"
	"github.com/tellor-io/telliot/pkg/logging"
	psrTellor "github.com/tellor-io/telliot/pkg/psr/tellor"
)

const ComponentName = "disputeTracker"

const reorgEventWait = 3 * time.Minute

type Config struct {
	LogLevel string
}

type Dispute struct {
	logger        log.Logger
	ctx           context.Context
	close         context.CancelFunc
	cfg           Config
	tsDB          *tsdb.DB
	client        contracts.ETHClient
	contract      *contracts.ITellor
	pendingAppend map[string]context.CancelFunc
	mtx           sync.Mutex
	psrTellor     *psrTellor.Psr
}

func New(
	logger log.Logger,
	ctx context.Context,
	cfg Config,
	tsDB *tsdb.DB,
	client contracts.ETHClient,
	contract *contracts.ITellor,
	psrTellor *psrTellor.Psr,
) (*Dispute, error) {
	logger, err := logging.ApplyFilter(cfg.LogLevel, logger)
	if err != nil {
		return nil, errors.Wrap(err, "apply filter logger")
	}
	logger = log.With(logger, "component", ComponentName)
	ctx, close := context.WithCancel(ctx)

	return &Dispute{
		client:        client,
		contract:      contract,
		psrTellor:     psrTellor,
		cfg:           cfg,
		ctx:           ctx,
		close:         close,
		tsDB:          tsDB,
		logger:        logger,
		pendingAppend: make(map[string]context.CancelFunc),
	}, nil
}

func (self *Dispute) Start() {
	var err error
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	logger := log.With(self.logger, "contract", "tellor")

	var sub event.Subscription
	events := make(chan *tellor.TellorNonceSubmitted)

	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}
		sub, err = self.newSubTellor(events)
		if err != nil {
			level.Error(logger).Log("msg", "initial subscribing to events failed")
			<-ticker.C
			continue
		}
		break
	}

	for {
		select {
		case <-self.ctx.Done():
			return
		case err := <-sub.Err():
			if err != nil {
				level.Error(logger).Log(
					"msg",
					"subscription error",
					"err", err)
			}

			// Trying to resubscribe until it succeeds.
			for {
				select {
				case <-self.ctx.Done():
					return
				default:
				}
				sub, err = self.newSubTellor(events)
				if err != nil {
					level.Error(logger).Log("msg", "re-subscribing to events failed", "err", err)
					<-ticker.C
					continue
				}
				break
			}
			level.Info(logger).Log("msg", "re-subscribed to events")
		case event := <-events:
			level.Debug(self.logger).Log(
				"msg", "new event",
				"removed", event.Raw.Removed,
				"hash", event.Raw.TxHash.String()[:8],
				"miner", event.Miner.String()[:8],
			)
			if event.Raw.Removed {
				self.removePending(event)
			}

			ctx, cncl := context.WithCancel(self.ctx)
			self.mtx.Lock()
			self.pendingAppend[event.Raw.TxHash.String()] = cncl
			self.mtx.Unlock()

			go func(ctx context.Context) {
				ticker := time.NewTicker(reorgEventWait) // Wait this long for any re-org events that can cancel this append.
				defer ticker.Stop()

				select {
				case <-ticker.C:
					if err := self.addValTellor(event); err != nil {
						level.Error(logger).Log(
							"msg", "adding value",
							"err", err,
						)
					}
					self.removePending(event)
				case <-ctx.Done():
					level.Debug(self.logger).Log("msg", "append canceled", "hash", event.Raw.TxHash.String()[:8])
					return
				}
			}(ctx)
		}
	}
}

// removePending is extracted in a separate function to use defer for unlocking the mutex and
// avoid forgetting to unlock it for early returns.
func (self *Dispute) removePending(event *tellor.TellorNonceSubmitted) {
	self.mtx.Lock()
	defer self.mtx.Unlock()
	pendingCncl, ok := self.pendingAppend[event.Raw.TxHash.String()]
	if !ok {
		level.Error(self.logger).Log("msg", "missing pending TX for removed event")
		return
	}
	pendingCncl()
	delete(self.pendingAppend, event.Raw.TxHash.String())
}

func (self *Dispute) Stop() {
	self.close()
}

func (self *Dispute) addValTellor(event *tellor.TellorNonceSubmitted) (err error) {
	appender := self.tsDB.Appender(self.ctx)
	defer func() { // An appender always needs to be committed or rolled back.
		if err != nil {
			if err := appender.Rollback(); err != nil {
				level.Error(self.logger).Log("msg", "db rollback failed", "err", err)
			}
			return
		}
		if errC := appender.Commit(); errC != nil {
			err = errors.Wrap(err, "db append commit failed")
		}
	}()

	for i, valAct := range event.Value {
		ts := timestamp.FromTime(time.Now())
		lbls := labels.Labels{
			labels.Label{Name: "__name__", Value: "oracle_value"},
			labels.Label{Name: "contract", Value: "tellor"},
			labels.Label{Name: "id", Value: event.RequestId[i].String()},
			labels.Label{Name: "miner", Value: event.Miner.String()},
		}

		sort.Sort(lbls) // This is important! The labels need to be sorted to avoid creating the same series with duplicate reference.

		_, err = appender.Append(0, lbls, ts, float64(valAct.Int64()))
		if err != nil {
			return errors.Wrap(err, "append values to the DB")
		}

		valExp, err := self.psrTellor.GetValue(event.RequestId[i].Int64(), time.Now().Add(-reorgEventWait))
		if err != nil {
			return errors.Wrapf(err, "getting value from the PSR id:%v", event.RequestId[i].Int64())
		}

		lbls = labels.Labels{
			labels.Label{Name: "__name__", Value: "psr_value"},
			labels.Label{Name: "contract", Value: "tellor"},
			labels.Label{Name: "id", Value: event.RequestId[i].String()},
		}

		sort.Sort(lbls) // This is important! The labels need to be sorted to avoid creating the same series with duplicate reference.

		_, err = appender.Append(0, lbls, ts, float64(valExp))
		if err != nil {
			return errors.Wrap(err, "append values to the DB")
		}

		level.Debug(self.logger).Log(
			"msg", "added dispute tracker values",
			"id", event.RequestId[i].String(),
			"miner", event.Miner.String(),
			"oracleValue", valAct,
			"psrValue", valExp,
			"difference", ((float64(valExp)-float64(valAct.Int64()))/float64(valExp))*100,
		)
	}
	return nil
}

func (self *Dispute) newSubTellor(output chan *tellor.TellorNonceSubmitted) (event.Subscription, error) {
	tellorFilterer, err := tellor.NewTellorFilterer(self.contract.Address, self.client)
	if err != nil {
		return nil, errors.Wrap(err, "getting instance")
	}
	sub, err := tellorFilterer.WatchNonceSubmitted(&bind.WatchOpts{Context: self.ctx}, output, nil, nil)
	if err != nil {
		return nil, errors.Wrap(err, "getting channel")
	}
	return sub, nil
}
