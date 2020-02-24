package submitter

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"time"

	ethabi "github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/renproject/darknode/abi"
	"github.com/renproject/darknode/jsonrpc"
	"github.com/renproject/lightnode/db"
	"github.com/renproject/lightnode/http"
	"github.com/renproject/mercury/sdk/client/ethclient"
	"github.com/renproject/mercury/types/ethtypes"
	"github.com/renproject/phi"
	"github.com/sirupsen/logrus"
)

type input struct {
	ctx context.Context
	tx  abi.Tx
}

// Submitter polls txs which have been executed with a payload from the database.
// It tries to submit the txs to the GaaS contract.
type Submitter struct {
	logger       logrus.FieldLogger
	dispatcher   phi.Sender
	database     db.DB
	client       ethclient.Client
	key          *ecdsa.PrivateKey
	txs          chan input
	pollInterval time.Duration
}

func New(logger logrus.FieldLogger, dispatcher phi.Sender, database db.DB, client ethclient.Client, key *ecdsa.PrivateKey, pollInterval time.Duration) Submitter {
	return Submitter{
		logger:       logger,
		dispatcher:   dispatcher,
		database:     database,
		client:       client,
		key:          key,
		txs:          make(chan input, 128),
		pollInterval: pollInterval,
	}
}

func (sub Submitter) Run(ctx context.Context) {
	phi.ParBegin(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case tx := <-sub.txs:
				sub.submitTx(tx)
			}
		}
	}, func() {
		ticker := time.NewTicker(sub.pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sub.queryTx(ctx)
			}
		}
	})
}

func (sub Submitter) queryTx(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, sub.pollInterval)
	defer cancel()

	// Get unsubmitted tx hashes from the database.
	txs, err := sub.database.TxsWithStatus(db.TxStatusConfirmed, 24 *time.Hour, "")
	if err != nil {
		sub.logger.Errorf("[submitter] failed to read unsubmitted txs from database: %v", err)
		return
	}

	// Query the status of the tx
	phi.ParForAll(txs, func(i int) {
		status, tx, err := sub.queryStatus(ctx, txs[i].Hash)
		if err != nil {
			sub.logger.Errorf("cannot get status of tx = %v, err = %v", txs[i].Hash.String(), err)
			return
		}
		if status != "done" {
			return
		}

		// Send the tx to another background goroutine for submission.
		in := input{
			ctx: ctx,
			tx:  tx,
		}
		select {
		case <-ctx.Done():
		case sub.txs <- in:
		}
	})
}

// query the status of the tx.
func (sub Submitter) queryStatus(ctx context.Context, hash abi.B32) (string, abi.Tx, error) {
	queryTx := jsonrpc.ParamsQueryTx{TxHash: hash}
	data, err := json.Marshal(queryTx)
	if err != nil {
		return "", abi.Tx{}, err
	}

	// Send to dispatcher and wait for response
	req := http.NewRequestWithResponder(ctx, jsonrpc.Request{
		Version: "2.0",
		ID:      rand.Int31(),
		Method:  jsonrpc.MethodQueryTx,
		Params:  data,
	}, "")
	for !sub.dispatcher.Send(req) {
		sub.logger.Errorf("[submitter] cannot send query tx request to dispatcher")
		time.Sleep(time.Second)
	}

	// Waiting for response from the darknode.
	select {
	case response := <-req.Responder:
		if response.Error != nil {
			return "", abi.Tx{}, fmt.Errorf("fail to get status of tx = %v, code = %v, err = %v", hash.String(), response.Error.Code, response.Error.Message)
		}
		data, err := json.Marshal(response.Result)
		if err != nil {
			return "", abi.Tx{}, err
		}

		var result jsonrpc.ResponseQueryTx
		if err := json.Unmarshal(data, &result); err != nil {
			return "", abi.Tx{}, err
		}
		return result.TxStatus, result.Tx, nil
	case <-ctx.Done():
		return "", abi.Tx{}, ctx.Err()
	}
}

func (sub Submitter) submitTx(in input) {
	// Read payload and construct a signature from the r,s,v.
	payloadArg := in.tx.In.Get("payload")
	payload, ok := payloadArg.Value.(abi.ExtEthCompatPayload)
	if !ok {
		sub.logger.Errorf("[submitter] no payload in the tx")
		return
	}

	// Construct the params from the payload and signature.
	toArg := in.tx.In.Get("to")
	to := toArg.Value.(abi.ExtEthCompatAddress)
	contract, err := ethtypes.NewContract(sub.client.EthClient(), ethtypes.Address(to), payload.ABI)
	if err != nil {
		return
	}
	from := ethtypes.AddressFromPublicKey(&sub.key.PublicKey)
	params, err := params(in.tx)
	if err != nil {
		return
	}

	unsignedTx, err := contract.BuildTx(in.ctx, from, string(payload.Fn), big.NewInt(0), params...)
	if err != nil {
		return
	}
	if err := unsignedTx.Sign(sub.key); err != nil {
		return
	}
	txHash, err := sub.client.PublishSignedTx(in.ctx, unsignedTx)
	if err != nil {
		return
	}
	sub.logger.Infof("successfully queryTx tx to Ethereum, hash = %x", txHash)

	// Update tx status in the database
	if err := sub.database.UpdateStatus(in.tx.Hash, db.TxStatusSubmitted); err != nil {
		return
	}
}

// params constructs the params for the Ethereum transaction. It first unpacks
// the data from payload to get a list of params and appends amount, nhash and
// signature to the end of params.
func params(tx abi.Tx) ([]interface{}, error) {

	// Read payload and construct signature from the r,s,v.
	sig := SigFromRSV(tx)
	payloadArg := tx.In.Get("payload")
	payload, ok := payloadArg.Value.(abi.ExtEthCompatPayload)
	if !ok {
		return nil, fmt.Errorf("no payload in the tx")
	}

	// Unpack the params from the value
	a, err := ethabi.JSON(bytes.NewBuffer(payload.ABI))
	if err != nil {
		return nil, err
	}
	fnName := string(payload.Fn)
	method, ok := a.Methods[fnName]
	if !ok {
		return nil, fmt.Errorf("invalid function name")
	}
	a.Methods[fnName] = removeInput(method, "_amount", "_nHash", "_sig")
	values := map[string]interface{}{}
	if err := a.UnpackIntoMap(values, fnName, payload.Value); err != nil {
		return nil, err
	}

	// Append the amount, nhash and signature after the params.
	params := make([]interface{}, len(values)+3)
	for _, arg := range a.Methods[fnName].Inputs {
		value, ok := values[arg.Name]
		if !ok {
			return nil, fmt.Errorf("missing argument = %v", arg.Name)
		}
		params = append(params, value)
	}

	amount := tx.In.Get("amount").Value.(abi.U256)
	nhash := tx.Autogen.Get("nhash").Value.(abi.B32)
	return append(params, amount.Int, nhash, sig), nil
}

func removeInput(method ethabi.Method, names ...string) ethabi.Method {
	m := map[string]struct{}{}
	for _, name := range names {
		m[name] = struct{}{}
	}
	for i := range method.Inputs {
		if _, ok := m[method.Inputs[i].Name]; ok {
			method.Inputs = append(method.Inputs[:i], method.Inputs[i+1:]...)
		}
	}

	return method
}

func SigFromRSV(tx abi.Tx) []byte {
	rArg := tx.Out.Get("r")
	r := rArg.Value.(abi.B)
	sArg := tx.Out.Get("s")
	s := sArg.Value.(abi.B)
	vArg := tx.Out.Get("v")
	v := vArg.Value.(abi.B)
	v[0] += 27

	return append(append(r, s...), v...)
}