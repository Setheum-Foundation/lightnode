package watcher

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/go-redis/redis/v7"
	"github.com/jbenet/go-base58"
	"github.com/renproject/darknode/jsonrpc"
	"github.com/renproject/darknode/tx"
	"github.com/renproject/darknode/txengine"
	"github.com/renproject/darknode/txengine/txenginebindings/ethereumbindings"
	"github.com/renproject/id"
	v0 "github.com/renproject/lightnode/compat/v0"
	"github.com/renproject/multichain"
	"github.com/renproject/multichain/chain/bitcoin"
	"github.com/renproject/multichain/chain/bitcoincash"
	"github.com/renproject/multichain/chain/zcash"
	"github.com/renproject/pack"
	"github.com/sirupsen/logrus"
)

type BurnLogResult struct {
	Result ethereumbindings.MintGatewayLogicV1LogBurn
	Error  error
}

type BurnLogFetcher interface {
	FetchBurnLogs(ctx context.Context, from uint64, to uint64) (chan BurnLogResult, error)
}

type EthBurnLogFetcher struct {
	bindings *ethereumbindings.MintGatewayLogicV1
}

func NewBurnLogFetcher(bindings *ethereumbindings.MintGatewayLogicV1) EthBurnLogFetcher {
	return EthBurnLogFetcher{
		bindings: bindings,
	}
}

// This will fetch the burn event logs using the ethereum bindings and emit them via a channel
// We do this so that we can unit test the log handling without calling ethereum
func (fetcher EthBurnLogFetcher) FetchBurnLogs(ctx context.Context, from uint64, to uint64) (chan BurnLogResult, error) {
	iter, err := fetcher.bindings.FilterLogBurn(
		&bind.FilterOpts{
			Context: ctx,
			Start:   from,
			End:     &to,
		},
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}
	resultChan := make(chan BurnLogResult)

	go func() {
		func() {
			for iter.Next() {
				resultChan <- BurnLogResult{Result: *iter.Event}
				select {
				case <-ctx.Done():
					return
				}
			}
		}()
		// Iter should stop if an error occurs,
		// so no need to check on each iteration
		err := iter.Error()
		if err != nil {
			resultChan <- BurnLogResult{Error: err}
		}
		// Always close the iter because apparently
		// it doesn't close its subscription?
		err = iter.Close()
		if err != nil {
			resultChan <- BurnLogResult{Error: err}
		}

		close(resultChan)
	}()

	return resultChan, iter.Error()
}

// Watcher watches for event logs for burn transactions. These transactions are
// then forwarded to the cacher.
type Watcher struct {
	network            multichain.Network
	logger             logrus.FieldLogger
	gpubkey            pack.Bytes
	selector           tx.Selector
	bindings           txengine.Bindings
	ethClient          *ethclient.Client
	burnLogFetcher     BurnLogFetcher
	resolver           jsonrpc.Resolver
	cache              redis.Cmdable
	pollInterval       time.Duration
	maxBlockAdvance    uint64
	confidenceInterval uint64
}

// NewWatcher returns a new Watcher.
func NewWatcher(logger logrus.FieldLogger, network multichain.Network, selector tx.Selector, bindings txengine.Bindings, ethClient *ethclient.Client, burnLogFetcher BurnLogFetcher, resolver jsonrpc.Resolver, cache redis.Cmdable, distPubKey *id.PubKey, pollInterval time.Duration) Watcher {
	gpubkey := (*btcec.PublicKey)(distPubKey).SerializeCompressed()
	return Watcher{
		logger:             logger,
		network:            network,
		gpubkey:            gpubkey,
		selector:           selector,
		bindings:           bindings,
		ethClient:          ethClient,
		burnLogFetcher:     burnLogFetcher,
		resolver:           resolver,
		cache:              cache,
		pollInterval:       pollInterval,
		maxBlockAdvance:    1000,
		confidenceInterval: 6,
	}
}

// Run starts the watcher until the context is canceled.
func (watcher Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(watcher.pollInterval)
	defer ticker.Stop()

	for {
		watcher.watchLogShiftOuts(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// watchLogShiftOuts checks logs that have occurred between current block number
// and the last checked block number. It constructs a `jsonrpc.Request` from
// these events and forwards them to the resolver.
func (watcher Watcher) watchLogShiftOuts(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, watcher.pollInterval)
	defer cancel()

	// Get current block number and last checked block number.
	cur, err := watcher.currentBlockNumber(ctx)
	if err != nil {
		watcher.logger.Errorf("[watcher] error loading eth block header: %v", err)
		return
	}
	last, err := watcher.lastCheckedBlockNumber(cur)
	if err != nil {
		watcher.logger.Errorf("[watcher] error loading last checked block number: %v", err)
		return
	}

	if cur <= last {
		watcher.logger.Warnf("[watcher] tried to process old blocks")
		// Make sure we do not process old events. This could occur if there is
		// an issue with the underlying blockchain node, for example if it needs
		// to resync.
		//
		// Processing old events is generally not an issue as the Lightnode will
		// drop transactions if it detects they have already been handled by the
		// Darknode, however in the case the transaction backlog builds up
		// substantially, it can cause the Lightnode to be rate limited by the
		// Darknode upon dispatching requests.
		return
	}

	// Only advance by a set number of blocks at a time to prevent over-subscription
	step := last + watcher.maxBlockAdvance
	if step < cur {
		cur = step
	}

	// avoid checking blocks that might have shuffled
	cur -= watcher.confidenceInterval

	// Fetch logs
	c, err := watcher.burnLogFetcher.FetchBurnLogs(ctx, last, cur)
	if err != nil {
		watcher.logger.Errorf("[watcher] error iterating LogBurn events from=%v to=%v: %v", last, cur, err)
		return
	}

	// Loop through the logs and check if there are burn events.
	for res := range c {
		if res.Error != nil {
			watcher.logger.Errorf("[watcher] error iterating LogBurn events from=%v to=%v: %v", last, cur, res.Error)
			return
		}
		event := res.Result

		to := event.To

		amount := event.Amount.Uint64()
		nonce := event.N.Uint64()
		watcher.logger.Infof("[watcher] detected burn for %v (to=%v, amount=%v, nonce=%v)", watcher.selector.String(), string(to), amount, nonce)

		var nonceBytes pack.Bytes32
		copy(nonceBytes[:], pack.NewU256FromU64(pack.NewU64(nonce)).Bytes())

		// Send the burn transaction to the resolver.
		params, err := watcher.burnToParams(event.Raw.TxHash.Bytes(), pack.NewU256FromU64(pack.NewU64(amount)), to, nonceBytes, watcher.gpubkey)
		if err != nil {
			watcher.logger.Errorf("[watcher] cannot get params from burn transaction (to=%v, amount=%v, nonce=%v): %v", to, amount, nonce, err)
			continue
		}
		response := watcher.resolver.SubmitTx(ctx, 0, &params, nil)
		if response.Error != nil {
			watcher.logger.Errorf("[watcher] invalid burn transaction %v: %v", params, response.Error.Message)
			continue
		}
	}

	if err := watcher.cache.Set(watcher.key(), cur, 0).Err(); err != nil {
		watcher.logger.Errorf("[watcher] error setting last checked block number in redis: %v", err)
		return
	}
}

// key returns the key that is used to store the last checked block.
func (watcher Watcher) key() string {
	return fmt.Sprintf("%v_lastCheckedBlock", watcher.selector.String())
}

// currentBlockNumber returns the current block number on Ethereum.
func (watcher Watcher) currentBlockNumber(ctx context.Context) (uint64, error) {
	currentBlock, err := watcher.ethClient.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, err
	}
	return currentBlock.Number.Uint64(), nil
}

// lastCheckedBlockNumber returns the last checked block number of Ethereum.
func (watcher Watcher) lastCheckedBlockNumber(currentBlockN uint64) (uint64, error) {
	last, err := watcher.cache.Get(watcher.key()).Uint64()
	// Initialise the pointer with current block number if it has not been yet.
	if err == redis.Nil {
		watcher.logger.Errorf("[watcher] last checked block number not initialised")
		if err := watcher.cache.Set(watcher.key(), currentBlockN-1, 0).Err(); err != nil {
			watcher.logger.Errorf("[watcher] cannot initialise last checked block in redis: %v", err)
			return 0, err
		}
		return currentBlockN - 1, nil
	}
	return last, err
}

// burnToParams constructs params for a SubmitTx request with given ref.
func (watcher Watcher) burnToParams(txid pack.Bytes, amount pack.U256, toBytes []byte, nonce pack.Bytes32, gpubkey pack.Bytes) (jsonrpc.ParamsSubmitTx, error) {

	// For v0 burn, `to` can be base58 encoded
	version := tx.Version1
	to := string(toBytes)
	switch watcher.selector.Asset() {
	case multichain.BTC, multichain.BCH, multichain.ZEC:
		decoder := AddressEncodeDecoder(watcher.selector.Asset().OriginChain(), watcher.network)
		_, err := decoder.DecodeAddress(multichain.Address(to))
		if err != nil {
			to = base58.Encode(toBytes)
			_, err = decoder.DecodeAddress(multichain.Address(to))
			if err != nil {
				return jsonrpc.ParamsSubmitTx{}, err
			}
			version = tx.Version0
		}
	}

	burnChain := watcher.selector.Destination()
	toBytes, err := watcher.bindings.DecodeAddress(burnChain, multichain.Address(to))
	if err != nil {
		return jsonrpc.ParamsSubmitTx{}, err
	}

	txindex := pack.U32(0)
	payload := pack.Bytes{}
	phash := txengine.Phash(payload)
	nhash := txengine.Nhash(nonce, txid, txindex)
	ghash := txengine.Ghash(watcher.selector, phash, toBytes, nonce)
	input, err := pack.Encode(txengine.CrossChainInput{
		Txid:    txid,
		Txindex: txindex,
		Amount:  amount,
		Payload: payload,
		Phash:   phash,
		To:      pack.String(to),
		Nonce:   nonce,
		Nhash:   nhash,
		Gpubkey: gpubkey,
		Ghash:   ghash,
	})
	if err != nil {
		return jsonrpc.ParamsSubmitTx{}, err
	}
	hash, err := tx.NewTxHash(version, watcher.selector, pack.Typed(input.(pack.Struct)))
	if err != nil {
		return jsonrpc.ParamsSubmitTx{}, err
	}
	transaction := tx.Tx{
		Hash:     hash,
		Version:  version,
		Selector: watcher.selector,
		Input:    pack.Typed(input.(pack.Struct)),
	}

	// Map the v0 burn txhash to v1 txhash so that it is still
	// queryable
	// We don't get the required data during tx submission rpc to track it there,
	// so we persist here in order to not re-filter all burn events
	v0Hash := v0.BurnTxHash(watcher.selector, pack.NewU256(nonce))
	watcher.cache.Set(v0Hash.String(), transaction.Hash.String(), 0)

	// Map the selector + burn ref to the v0 hash so that we can return something
	// to ren-js v1
	watcher.cache.Set(fmt.Sprintf("%s_%v", watcher.selector, pack.NewU256(nonce).String()), v0Hash.String(), 0)

	return jsonrpc.ParamsSubmitTx{Tx: transaction}, nil
}

func AddressEncodeDecoder(chain multichain.Chain, network multichain.Network) multichain.AddressEncodeDecoder {
	switch chain {
	case multichain.Bitcoin, multichain.DigiByte, multichain.Dogecoin:
		params := NetParams(network, chain)
		return bitcoin.NewAddressEncodeDecoder(params)
	case multichain.BitcoinCash:
		params := NetParams(network, chain)
		return bitcoincash.NewAddressEncodeDecoder(params)
	case multichain.Zcash:
		params := ZcashNetParams(network)
		return zcash.NewAddressEncodeDecoder(params)
	default:
		panic(fmt.Errorf("unknown chain %v", chain))
	}
}

func ZcashNetParams(network multichain.Network) *zcash.Params {
	switch network {
	case multichain.NetworkMainnet:
		return &zcash.MainNetParams
	case multichain.NetworkDevnet, multichain.NetworkTestnet:
		return &zcash.TestNet3Params
	default:
		return &zcash.RegressionNetParams
	}
}

func NetParams(network multichain.Network, chain multichain.Chain) *chaincfg.Params {
	switch chain {
	case multichain.Bitcoin, multichain.BitcoinCash:
		switch network {
		case multichain.NetworkMainnet:
			return &chaincfg.MainNetParams
		case multichain.NetworkDevnet, multichain.NetworkTestnet:
			return &chaincfg.TestNet3Params
		default:
			return &chaincfg.RegressionNetParams
		}
	default:
		panic(fmt.Errorf("cannot get network params: unknown chain %v", chain))
	}
}
