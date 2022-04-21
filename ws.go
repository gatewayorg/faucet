package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/gorilla/websocket"
	"github.com/sunvim/utils/log"
)

// wsConn wraps a websocket connection with a write mutex as the underlying
// websocket library does not synchronize access to the stream.
type wsConn struct {
	conn  *websocket.Conn
	wlock sync.Mutex
}

var (
	faucet = struct {
		lock     sync.RWMutex
		conns    []*wsConn
		timeouts map[string]time.Time
		client   *ethclient.Client
	}{
		conns:    make([]*wsConn, 0, 1024),
		timeouts: make(map[string]time.Time),
	}
	err         error
	privateKey  *ecdsa.PrivateKey
	fromAddress common.Address
)

func initFaucet() {
	faucet.client, err = ethclient.Dial(*rpc)
	if err != nil {
		log.Fatal("init chain connect: ", err)
	}
	privateKey, err = crypto.HexToECDSA(*priKey)
	if err != nil {
		log.Fatal(err)
	}

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Fatal("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
	}

	fromAddress = crypto.PubkeyToAddress(*publicKeyECDSA)
}

func SendTx(amount *big.Int, toAddress string) error {
	ctx := context.Background()
	nonce, err := faucet.client.PendingNonceAt(ctx, fromAddress)
	if err != nil {
		log.Error(err)
		return err
	}

	gasLimit := uint64(21000) // in units
	gasPrice, err := faucet.client.SuggestGasPrice(context.Background())
	if err != nil {
		log.Error(err)
		return err
	}
	to := common.HexToAddress(toAddress)
	var data []byte
	tx := types.NewTransaction(nonce, to, amount, gasLimit, gasPrice, data)
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(big.NewInt(*chainID)), privateKey)
	if err != nil {
		log.Error(err)
		return err
	}

	log.Info("tx hash: ", signedTx.Hash().Hex())

	return faucet.client.SendTransaction(ctx, signedTx)
}

func OnWebsocket(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// Start tracking the connection and drop at the end
	defer conn.Close()

	faucet.lock.Lock()
	wsconn := &wsConn{conn: conn}
	faucet.conns = append(faucet.conns, wsconn)
	faucet.lock.Unlock()

	defer func() {
		faucet.lock.Lock()
		for i, c := range faucet.conns {
			if c.conn == conn {
				faucet.conns = append(faucet.conns[:i], faucet.conns[i+1:]...)
				break
			}
		}
		faucet.lock.Unlock()
	}()

	for {
		// Fetch the next funding request and validate against github
		var msg struct {
			URL     string `json:"url"`
			Tier    uint   `json:"tier"`
			Captcha string `json:"captcha"`
		}
		if err = conn.ReadJSON(&msg); err != nil {
			return
		}
		if msg.Tier >= uint(*tiersFlag) {
			//lint:ignore ST1005 This error is to be displayed in the browser
			if err = sendError(wsconn, errors.New("Invalid funding tier requested")); err != nil {
				log.Error("Failed to send tier error to client", "err", err)
				return
			}
			continue
		}
		log.Info("Faucet funds requested: ", "url: ", msg.URL, " tier: ", msg.Tier)
		// Ensure the user didn't request funds too recently
		faucet.lock.Lock()
		var (
			fund    bool
			timeout time.Time
		)
		if timeout = faucet.timeouts[msg.URL]; time.Now().After(timeout) {
			// User wasn't funded recently, create the funding transaction
			amount := new(big.Int).Mul(big.NewInt(int64(*payoutFlag)), ether)
			amount = new(big.Int).Mul(amount, new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(msg.Tier)), nil))
			amount = new(big.Int).Div(amount, new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(msg.Tier)), nil))

			// Submit the transaction and mark as funded if successful
			if err := SendTx(amount, msg.URL); err != nil {
				faucet.lock.Unlock()
				if err = sendError(wsconn, err); err != nil {
					log.Error("Failed to send transaction transmission error to client err", err)
					return
				}
				continue
			}
			timeout := time.Duration(*minutesFlag*int(math.Pow(3, float64(msg.Tier)))) * time.Minute
			grace := timeout / 288 // 24h timeout => 5m grace

			faucet.timeouts[msg.URL] = time.Now().Add(timeout - grace)
			fund = true
		}
		faucet.lock.Unlock()

		// Send an error if too frequent funding, othewise a success
		if !fund {
			if err = sendError(wsconn, fmt.Errorf("%s left until next allowance", common.PrettyDuration(time.Until(timeout)))); err != nil { // nolint: gosimple
				log.Error("Failed to send funding error to client err: ", err)
				return
			}
			continue
		}
		if err = sendSuccess(wsconn, fmt.Sprintf("Funding request accepted for Faucet into %s", msg.URL)); err != nil {
			log.Error("Failed to send funding success to client err", err)
			return
		}
	}

}

// sendError transmits an error to the remote end of the websocket, also setting
// the write deadline to 1 second to prevent waiting forever.
func sendError(conn *wsConn, err error) error {
	return send(conn, map[string]string{"error": err.Error()}, time.Second)
}

// sendSuccess transmits a success message to the remote end of the websocket, also
// setting the write deadline to 1 second to prevent waiting forever.
func sendSuccess(conn *wsConn, msg string) error {
	return send(conn, map[string]string{"success": msg}, time.Second)
}

// sends transmits a data packet to the remote end of the websocket, but also
// setting a write deadline to prevent waiting forever on the node.
func send(conn *wsConn, value interface{}, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	conn.wlock.Lock()
	defer conn.wlock.Unlock()
	conn.conn.SetWriteDeadline(time.Now().Add(timeout))
	return conn.conn.WriteJSON(value)
}
