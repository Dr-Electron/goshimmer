package connector

import (
	"io"
	"net"
	"strings"

	"github.com/iotaledger/goshimmer/dapps/valuetransfers/packages/address"
	"github.com/iotaledger/goshimmer/dapps/valuetransfers/packages/balance"
	"github.com/iotaledger/goshimmer/dapps/valuetransfers/packages/transaction"
	"github.com/iotaledger/goshimmer/dapps/waspconn/packages/valuetangle"
	"github.com/iotaledger/goshimmer/dapps/waspconn/packages/waspconn"
	"github.com/iotaledger/goshimmer/packages/binary/messagelayer/payload"
	"github.com/iotaledger/goshimmer/packages/shutdown"
	"github.com/iotaledger/hive.go/daemon"
	"github.com/iotaledger/hive.go/events"
	"github.com/iotaledger/hive.go/logger"
	"github.com/iotaledger/hive.go/netutil/buffconn"
)

type WaspConnector struct {
	id                                 string
	bconn                              *buffconn.BufferedConnection
	subscriptions                      map[address.Address]int
	inTxChan                           chan interface{}
	exitConnChan                       chan struct{}
	receiveConfirmedTransactionClosure *events.Closure
	receiveBookedTransactionClosure    *events.Closure
	receiveRejectedTransactionClosure  *events.Closure
	receiveWaspMessageClosure          *events.Closure
	log                                *logger.Logger
	vtangle                            valuetangle.ValueTangle
}

type wrapConfirmedTx *transaction.Transaction
type wrapBookedTx *transaction.Transaction
type wrapRejectedTx *transaction.Transaction

func Run(conn net.Conn, log *logger.Logger, vtangle valuetangle.ValueTangle) {
	wconn := &WaspConnector{
		bconn:        buffconn.NewBufferedConnection(conn, payload.MaxMessageSize),
		exitConnChan: make(chan struct{}),
		log:          log,
		vtangle:      vtangle,
	}
	err := daemon.BackgroundWorker(wconn.Id(), func(shutdownSignal <-chan struct{}) {
		select {
		case <-shutdownSignal:
			wconn.log.Infof("shutdown signal received..")
			_ = wconn.bconn.Close()

		case <-wconn.exitConnChan:
			wconn.log.Infof("closing connection..")
			_ = wconn.bconn.Close()
		}

		go wconn.detach()
	}, shutdown.PriorityWaspConn)

	if err != nil {
		close(wconn.exitConnChan)
		wconn.log.Errorf("can't start deamon")
		return
	}
	wconn.attach()
}

func (wconn *WaspConnector) Id() string {
	if wconn.id == "" {
		return "wasp_" + wconn.bconn.RemoteAddr().String()
	}
	return wconn.id
}

func (wconn *WaspConnector) SetId(id string) {
	wconn.id = id
	wconn.log = wconn.log.Named(id)
	wconn.log.Infof("wasp connection id has been set to '%s' for '%s'", id, wconn.bconn.RemoteAddr().String())
}

func (wconn *WaspConnector) attach() {
	wconn.subscriptions = make(map[address.Address]int)
	wconn.inTxChan = make(chan interface{})

	wconn.receiveConfirmedTransactionClosure = events.NewClosure(func(vtx *transaction.Transaction) {
		wconn.inTxChan <- wrapConfirmedTx(vtx)
	})

	wconn.receiveBookedTransactionClosure = events.NewClosure(func(vtx *transaction.Transaction) {
		wconn.inTxChan <- wrapBookedTx(vtx)
	})

	wconn.receiveRejectedTransactionClosure = events.NewClosure(func(vtx *transaction.Transaction) {
		wconn.inTxChan <- wrapRejectedTx(vtx)
	})

	wconn.receiveWaspMessageClosure = events.NewClosure(func(data []byte) {
		wconn.processMsgDataFromWasp(data)
	})

	// attach connector to the flow of incoming value transactions
	EventValueTransactionConfirmed.Attach(wconn.receiveConfirmedTransactionClosure)
	EventValueTransactionBooked.Attach(wconn.receiveBookedTransactionClosure)
	EventValueTransactionRejected.Attach(wconn.receiveRejectedTransactionClosure)

	wconn.bconn.Events.ReceiveMessage.Attach(wconn.receiveWaspMessageClosure)

	wconn.log.Debugf("attached waspconn")

	// read connection thread
	go func() {
		if err := wconn.bconn.Read(); err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
				wconn.log.Warnw("Permanent error", "err", err)
			}
		}
		close(wconn.exitConnChan)
	}()

	// read incoming pre-filtered transactions from node
	go func() {
		for vtx := range wconn.inTxChan {
			switch tvtx := vtx.(type) {
			case wrapConfirmedTx:
				wconn.processConfirmedTransactionFromNode(tvtx)

			case wrapBookedTx:
				wconn.processBookedTransactionFromNode(tvtx)

			case wrapRejectedTx:
				wconn.processRejectedTransactionFromNode(tvtx)

			default:
				wconn.log.Panicf("wrong type")
			}
		}
	}()
}

func (wconn *WaspConnector) detach() {
	EventValueTransactionConfirmed.Detach(wconn.receiveConfirmedTransactionClosure)
	EventValueTransactionBooked.Detach(wconn.receiveBookedTransactionClosure)
	EventValueTransactionRejected.Detach(wconn.receiveRejectedTransactionClosure)
	wconn.bconn.Events.ReceiveMessage.Detach(wconn.receiveWaspMessageClosure)

	close(wconn.inTxChan)
	_ = wconn.bconn.Close()

	wconn.log.Debugf("detached waspconn")
}

func (wconn *WaspConnector) subscribe(addr *address.Address) {
	_, ok := wconn.subscriptions[*addr]
	if !ok {
		wconn.log.Debugf("subscribed to address: %s", addr.String())
		wconn.subscriptions[*addr] = 0
	}
}

func (wconn *WaspConnector) isSubscribed(addr *address.Address) bool {
	_, ok := wconn.subscriptions[*addr]
	return ok
}

func (wconn *WaspConnector) txSubscribedAddresses(tx *transaction.Transaction) []address.Address {
	ret := make([]address.Address, 0)
	tx.Outputs().ForEach(func(addr address.Address, _ []*balance.Balance) bool {
		if wconn.isSubscribed(&addr) {
			ret = append(ret, addr)
		}
		return true
	})
	return ret
}

// processConfirmedTransactionFromNode receives only confirmed transactions
// it parses SC transaction incoming from the node. Forwards it to Wasp if subscribed
func (wconn *WaspConnector) processConfirmedTransactionFromNode(tx *transaction.Transaction) {
	// determine if transaction contains any of subscribed addresses in its outputs
	wconn.log.Debugw("processConfirmedTransactionFromNode", "txid", tx.ID().String())

	subscribedOutAddresses := wconn.txSubscribedAddresses(tx)
	if len(subscribedOutAddresses) == 0 {
		wconn.log.Debugw("not subscribed", "txid", tx.ID().String())
		// dismiss unsubscribed transaction
		return
	}
	// for each subscribed address retrieve outputs and send to wasp with the transaction
	wconn.log.Debugf("txid %s contains %d subscribed addresses", tx.ID().String(), len(subscribedOutAddresses))

	for i := range subscribedOutAddresses {
		outs, err := wconn.vtangle.GetConfirmedAddressOutputs(subscribedOutAddresses[i])
		if err != nil {
			wconn.log.Error(err)
			continue
		}
		err = wconn.sendAddressUpdateToWasp(
			&subscribedOutAddresses[i],
			waspconn.OutputsToBalances(outs),
			tx,
		)
		if err != nil {
			wconn.log.Error(err)
		} else {
			wconn.log.Infof("confirmed tx -> Wasp: sc addr: %s, txid: %s",
				subscribedOutAddresses[i].String(), tx.ID().String())
		}
	}
}

func (wconn *WaspConnector) processBookedTransactionFromNode(tx *transaction.Transaction) {
	addrs := wconn.txSubscribedAddresses(tx)
	if len(addrs) == 0 {
		return
	}
	if err := wconn.sendTransactionEventToWasp(waspconn.TransactionEventBooked, tx.ID(), addrs); err != nil {
		wconn.log.Errorf("processBookedTransactionFromNode: %v", err)
	} else {
		wconn.log.Infof("booked tx -> Wasp. txid: %s", tx.ID().String())
	}
}

func (wconn *WaspConnector) processRejectedTransactionFromNode(tx *transaction.Transaction) {
	addrs := wconn.txSubscribedAddresses(tx)
	if len(addrs) == 0 {
		return
	}
	if err := wconn.sendTransactionEventToWasp(waspconn.TransactionEventRejected, tx.ID(), addrs); err != nil {
		wconn.log.Errorf("processRejectedTransactionFromNode: %v", err)
	} else {
		wconn.log.Infof("rejected tx -> Wasp. txid: %s", tx.ID().String())
	}
}

func (wconn *WaspConnector) getTransaction(txid *transaction.ID) {
	wconn.log.Debugf("requested transaction id = %s", txid.String())

	tx, confirmed := wconn.vtangle.GetTransaction(txid)
	if tx == nil {
		wconn.log.Warnf("GetTransaction %s : not found", txid.String())
		return
	}
	if err := wconn.sendTransactionToWasp(tx, confirmed); err != nil {
		wconn.log.Errorf("sendTransactionToWasp: %v", err)
		return
	}
	wconn.log.Infof("requested tx -> Wasp. txid = %s, confirmed = %v", txid.String(), confirmed)
}

func (wconn *WaspConnector) getAddressBalance(addr *address.Address) {
	wconn.log.Debugf("getAddressBalance request for address: %s", addr.String())

	outputs, err := wconn.vtangle.GetConfirmedAddressOutputs(*addr)
	if err != nil {
		panic(err)
	}
	if len(outputs) == 0 {
		return
	}
	ret := waspconn.OutputsToBalances(outputs)

	wconn.log.Debugf("sending balances to wasp: %s    %+v", addr.String(), ret)

	if err := wconn.sendAddressOutputsToWasp(addr, ret); err != nil {
		wconn.log.Debugf("sendAddressOutputsToWasp: %v", err)
	}
}

func (wconn *WaspConnector) postTransaction(tx *transaction.Transaction, fromSC *address.Address, fromLeader uint16) {
	if err := wconn.vtangle.PostTransaction(tx); err != nil {
		wconn.log.Warnf("%v: %s", err, tx.ID().String())
		return
	}
	wconn.log.Infof("Wasp -> Tangle. txid: %s, from sc: %s, from leader: %d",
		tx.ID().String(), fromSC.String(), fromLeader)
}
