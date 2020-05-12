package resolver

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"runtime"
	"sync"
	"time"

	"github.com/renproject/darknode/abi"
	"github.com/renproject/darknode/consensus/txcheck/transform"
	"github.com/renproject/darknode/jsonrpc"
	"github.com/renproject/lightnode/db"
	"github.com/renproject/lightnode/http"
	"github.com/renproject/phi"
	"github.com/sirupsen/logrus"
)

// A txChecker reads submitTx requests from a channel and validate the details
// of the tx. It will store the tx if it's valid.
type txChecker struct {
	mu        *sync.Mutex
	logger    logrus.FieldLogger
	requests  <-chan http.RequestWithResponder
	disPubkey ecdsa.PublicKey
	bc        transform.Blockchain
	db        db.DB
}

// newTxChecker returns a new txChecker.
func newTxChecker(logger logrus.FieldLogger, requests <-chan http.RequestWithResponder, key ecdsa.PublicKey, bc transform.Blockchain, db db.DB) txChecker {
	return txChecker{
		mu:        new(sync.Mutex),
		logger:    logger,
		requests:  requests,
		disPubkey: key,
		bc:        bc,
		db:        db,
	}
}

// Run starts the txChecker until the requests channel is closed.
func (tc *txChecker) Run() {
	workers := 2 * runtime.NumCPU()
	phi.ForAll(workers, func(_ int) {
		for req := range tc.requests {
			tx, err := tc.verify(req.Params.(jsonrpc.ParamsSubmitTx))
			if err != nil {
				req.RespondWithErr(jsonrpc.ErrorCodeInvalidParams, err)
				continue
			}

			// Check for duplicate.
			gaas := req.Query.Get("gaas")
			tx, err = tc.checkDuplicate(tx, gaas)
			if err != nil {
				tc.logger.Errorf("[txChecker] cannot check tx duplication, err = %v", err)
				req.RespondWithErr(jsonrpc.ErrorCodeInternal, err)
				continue
			}

			// Write the response to the responder channel.
			response := jsonrpc.ResponseSubmitTx{
				Tx: tx,
			}
			req.Responder <- jsonrpc.NewResponse(req.ID, response, nil)
		}
	})
}

func (tc *txChecker) verify(params jsonrpc.ParamsSubmitTx) (abi.Tx, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Verify the parameters
	if err := transform.ValidateTxParams(params.Tx); err != nil {
		return abi.Tx{}, err
	}

	// Validate the phash and calculate other hashes.
	tx, err := transform.PHash(params.Tx)
	if err != nil {
		return abi.Tx{}, err
	}
	tx = transform.GHash(tx)
	tx = transform.NHash(tx)
	tx = transform.TxHash(tx)

	// Validate the utxo or shiftOut event.
	if abi.IsShiftIn(tx.To) {
		tx, err = transform.ValidateUtxo(ctx, tc.bc, tx, 0, tc.disPubkey)
		if err != nil {
			return abi.Tx{}, err
		}
		return transform.Sighash(tx), nil
	} else {
		return transform.AddShiftOutDetails(ctx, tx, tc.bc, 0)
	}
}

func (tc *txChecker) checkDuplicate(tx abi.Tx, gaas string) (abi.Tx, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	stored, err := tc.db.Tx(tx.Hash, false)
	if err == sql.ErrNoRows {
		return tx, tc.db.InsertTx(tx, gaas != "")
	}
	return stored, err
}