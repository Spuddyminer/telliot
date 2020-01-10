package ops

import (
	"context"
	"fmt"
	tellorCommon "github.com/tellor-io/TellorMiner/common"
	"github.com/tellor-io/TellorMiner/config"
	"github.com/tellor-io/TellorMiner/db"
	"github.com/tellor-io/TellorMiner/pow"
	"github.com/tellor-io/TellorMiner/util"
	"log"
	"math"
	"math/rand"
	"os"
	"time"
)

//MiningMgr holds items for mining and requesting data
type MiningMgr struct {
	//primary exit channel
	exitCh  chan os.Signal
	log     *util.Logger
	Running bool

	group  *pow.MiningGroup
	tasker *pow.MiningTasker
	solHandler *pow.SolutionHandler

	proxy db.DataServerProxy

	dataRequester *DataRequester
	//data requester's exit channel
	requesterExit chan os.Signal
}

//CreateMiningManager creates a new manager that mananges mining and data requests
func CreateMiningManager(ctx context.Context, exitCh chan os.Signal, submitter tellorCommon.TransactionSubmitter) (*MiningMgr, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}

	group, err := pow.SetupMiningGroup(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to setup miners: %s", err.Error())
	}

	proxy := ctx.Value(tellorCommon.DataProxyKey).(db.DataServerProxy)

	tasker := pow.CreateTasker(cfg, proxy)
	solHandler := pow.CreateSolutionHandler(cfg, submitter, proxy)

	rExit := make(chan os.Signal)

	dataRequester := CreateDataRequester(rExit, submitter, cfg.RequestDataInterval.Duration, proxy)
	log := util.NewLogger("ops", "MiningMgr")

	return &MiningMgr{
		exitCh:  exitCh,
		log:     log,
		Running: false,
		group:   group,
		proxy:   proxy,
		tasker:  tasker,
		solHandler: solHandler,
		dataRequester: dataRequester,
		requesterExit: rExit}, nil
}

//Start will start the mining run loop
func (mgr *MiningMgr) Start(ctx context.Context) {
	go func(ctx context.Context) {
		cfg, err := config.GetConfig()
		if err != nil {
			log.Fatal(err)
		}


		ticker := time.NewTicker(cfg.MiningInterruptCheckInterval.Duration)

		input := make(chan *pow.Work)
		output := make(chan *pow.Result)

		//start the mining group
		go mgr.group.Mine(input, output)

		// sends work to the mining group
		sendWork := func () {
			//if its nil, nothing new to report
			challenge := mgr.tasker.PullUpdates()
			if challenge != nil {
				input <-&pow.Work{Challenge:challenge, Start:uint64(rand.Int63()), N:math.MaxInt64}
			}
		}
		//send the initial challenge
		sendWork()
		for {
			select {
			//boss wants us to quit for the day
			case <-mgr.exitCh:
				//exit
				input <- nil

			//found a solution
			case result := <- output:
				if result == nil {
					mgr.Running = false
					return
				}
				mgr.solHandler.HandleSolution(ctx, result.Work.Challenge, result.Nonce)

			//time to check for a new challenge
			case _ = <-ticker.C:
				sendWork()
			}
		}
	}(ctx)
}
