// package decision implements the decision engine for the bitswap service.
package decision

import (
	"context"
	"sync"
	"time"

	bsmsg "github.com/ipfs/go-bitswap/message"
	wl "github.com/ipfs/go-bitswap/wantlist"

	"fmt"
	"github.com/fatih/color"
	"github.com/ipfs/go-block-format"
	bstore "github.com/ipfs/go-ipfs-blockstore"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p-peer"
	"strings"
)

// TODO consider taking responsibility for other types of requests. For
// example, there could be a |cancelQueue| for all of the cancellation
// messages that need to go out. There could also be a |wantlistQueue| for
// the local peer's wantlists. Alternatively, these could all be bundled
// into a single, intelligent global queue that efficiently
// batches/combines and takes all of these into consideration.
//
// Right now, messages go onto the network for four reasons:
// 1. an initial `sendwantlist` message to a provider of the first key in a
//    request
// 2. a periodic full sweep of `sendwantlist` messages to all providers
// 3. upon receipt of blocks, a `cancel` message to all peers
// 4. draining the priority queue of `blockrequests` from peers
//
// Presently, only `blockrequests` are handled by the decision engine.
// However, there is an opportunity to give it more responsibility! If the
// decision engine is given responsibility for all of the others, it can
// intelligently decide how to combine requests efficiently.
//
// Some examples of what would be possible:
//
// * when sending out the wantlists, include `cancel` requests
// * when handling `blockrequests`, include `sendwantlist` and `cancel` as
//   appropriate
// * when handling `cancel`, if we recently received a wanted block from a
//   peer, include a partial wantlist that contains a few other high priority
//   blocks
//
// In a sense, if we treat the decision engine as a black box, it could do
// whatever it sees fit to produce desired outcomes (get wanted keys
// quickly, maintain good relationships with peers, etc).

var log = logging.Logger("engine")

const (
	// outboxChanBuffer must be 0 to prevent stale messages from being sent
	outboxChanBuffer = 0
)

// Envelope contains a message for a Peer
type Envelope struct {
	// Peer is the intended recipient
	Peer peer.ID

	// Block is the payload
	Block blocks.Block

	// A callback to notify the decision queue that the task is complete
	Sent func()
}

type Engine struct {
	// peerRequestQueue is a priority queue of requests received from peers.
	// Requests are popped from the queue, packaged up, and placed in the
	// outbox.
	peerRequestQueue *prq

	// FIXME it's a bit odd for the client and the worker to both share memory
	// (both modify the peerRequestQueue) and also to communicate over the
	// workSignal channel. consider sending requests over the channel and
	// allowing the worker to have exclusive access to the peerRequestQueue. In
	// that case, no lock would be required.
	workSignal chan struct{}

	// outbox contains outgoing messages to peers. This is owned by the
	// taskWorker goroutine
	outbox chan (<-chan *Envelope)

	bs bstore.Blockstore

	lock sync.Mutex // protects the fields immediatly below
	// ledgerMap lists Ledgers by their Partner key.
	ledgerMap map[peer.ID]*ledger

	ticker *time.Ticker

	last_sent_to peer.ID
}

func NewEngine(ctx context.Context, bs bstore.Blockstore) *Engine {
	e := &Engine{
		ledgerMap:        make(map[peer.ID]*ledger),
		bs:               bs,
		peerRequestQueue: newPRQ(),
		outbox:           make(chan (<-chan *Envelope), outboxChanBuffer),
		workSignal:       make(chan struct{}, 1),
		ticker:           time.NewTicker(time.Millisecond * 100),
		last_sent_to:	  peer.ID("QmWyntbo4FGSE5Zkrgf88g4X18gK5nBG1gWwrPurAXgS7a"),
	}
	color.Green("### John ### New engine created")
	go e.taskWorker(ctx)
	return e
}

func (e *Engine) WantlistForPeer(p peer.ID) (out []*wl.Entry) {
	partner := e.findOrCreate(p)
	partner.lk.Lock()
	defer partner.lk.Unlock()
	return partner.wantList.SortedEntries()
}

func (e *Engine) LedgerForPeer(p peer.ID) *Receipt {
	ledger := e.findOrCreate(p)

	ledger.lk.Lock()
	defer ledger.lk.Unlock()

	return &Receipt{
		Peer:      ledger.Partner.String(),
		Value:     ledger.Accounting.Value(),
		Sent:      ledger.Accounting.BytesSent,
		Recv:      ledger.Accounting.BytesRecv,
		Exchanged: ledger.ExchangeCount(),
	}
}

func (e *Engine) UnsafeReceiptFromLedger(ledger_ *ledger) *Receipt {

	return &Receipt{
		Peer:      ledger_.Partner.String(),
		Value:     ledger_.Accounting.Value(),
		Sent:      ledger_.Accounting.BytesSent,
		Recv:      ledger_.Accounting.BytesRecv,
		Exchanged: ledger_.ExchangeCount(),
	}
}

func (e *Engine) taskWorker(ctx context.Context) {
	defer close(e.outbox) // because taskWorker uses the channel exclusively
	for {
		oneTimeUse := make(chan *Envelope, 1) // buffer to prevent blocking
		select {
		case <-ctx.Done():
			return
		case e.outbox <- oneTimeUse:
		}
		// receiver is ready for an outoing envelope. let's prepare one. first,
		// we must acquire a task from the PQ...
		envelope, err := e.nextEnvelope(ctx)
		if err != nil {
			close(oneTimeUse)
			return // ctx cancelled
		}
		oneTimeUse <- envelope // buffered. won't block
		close(oneTimeUse)
	}
}

// nextEnvelope runs in the taskWorker goroutine. Returns an error if the
// context is cancelled before the next Envelope can be created.
func (e *Engine) nextEnvelope(ctx context.Context) (*Envelope, error) {
	for {
		nextTask := e.peerRequestQueue.Pop()
		for nextTask == nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-e.workSignal:
				nextTask = e.peerRequestQueue.Pop()
			case <-e.ticker.C:
				e.peerRequestQueue.thawRound()
				nextTask = e.peerRequestQueue.Pop()
			}
		}

		// with a task in hand, we're ready to prepare the envelope...

		block, err := e.bs.Get(nextTask.Entry.Cid)
		if err != nil {
			log.Errorf("tried to execute a task and errored fetching block: %s", err)
			// If we don't have the block, don't hold that against the peer
			// make sure to update that the task has been 'completed'
			nextTask.Done()
			continue
		}

		color.Yellow(fmt.Sprintf("### JOHN ### Sending blocks to: %v", nextTask.Target.String()))
		if e.last_sent_to != nextTask.Target {
			color.Yellow(">>> Switching target")
			e.last_sent_to = nextTask.Target
		}

		return &Envelope{
			Peer:  nextTask.Target,
			Block: block,
			Sent: func() {
				nextTask.Done()
				select {
				case e.workSignal <- struct{}{}:
					// work completing may mean that our queue will provide new
					// work to be done.
				default:
				}
			},
		}, nil
	}
}

// Outbox returns a channel of one-time use Envelope channels.
func (e *Engine) Outbox() <-chan (<-chan *Envelope) {
	return e.outbox
}

// Returns a slice of Peers with whom the local node has active sessions
func (e *Engine) Peers() []peer.ID {
	e.lock.Lock()
	defer e.lock.Unlock()

	response := make([]peer.ID, 0, len(e.ledgerMap))

	for _, ledger := range e.ledgerMap {
		response = append(response, ledger.Partner)
	}
	return response
}

// MessageReceived performs book-keeping. Returns error if passed invalid
// arguments.
func (e *Engine) MessageReceived(p peer.ID, m bsmsg.BitSwapMessage) error {
	if len(m.Wantlist()) == 0 && len(m.Blocks()) == 0 {
		log.Debugf("received empty message from %s", p)
	}

	newWorkExists := false
	defer func() {
		if newWorkExists {
			e.signalNewWork()
		}
	}()

	l := e.findOrCreate(p)

	//if strings.Contains(p.String(), "SZGgvN") {
	//	color.Magenta("### John ### Message received from " + p.String())
	//	color.Magenta(fmt.Sprintf("Message: %v", m.Wantlist()))
	//	color.Magenta(fmt.Sprintf("Ledger: %+v", l))
	//}

	l.lk.Lock()
	defer l.lk.Unlock()
	if m.Full() {
		l.wantList = wl.New()
	}

	for _, entry := range m.Wantlist() {
		if entry.Cancel {
			log.Debugf("%s cancel %s", p, entry.Cid)
			l.CancelWant(entry.Cid)
			e.peerRequestQueue.Remove(entry.Cid, p)
		} else {
			log.Debugf("wants %s - %d", entry.Cid, entry.Priority)
			l.Wants(entry.Cid, entry.Priority)
			if exists, err := e.bs.Has(entry.Cid); err == nil && exists {
				receipt := e.UnsafeReceiptFromLedger(l)
				//color.Green(fmt.Sprintf("Receipt: %+v", receipt))
				e.peerRequestQueue.Push(entry.Entry, p, receipt)
				newWorkExists = true
			}
		}
	}

	for _, block := range m.Blocks() {
		log.Debugf("got block %s %d bytes", block, len(block.RawData()))
		l.ReceivedBytes(len(block.RawData()))
	}
	return nil
}

func (e *Engine) addBlock(block blocks.Block) {
	work := false

	for _, l := range e.ledgerMap {
		l.lk.Lock()
		if entry, ok := l.WantListContains(block.Cid()); ok {
			// We might be reading Partner's ledger while a goroutine edits it ...
			receipt := e.UnsafeReceiptFromLedger(l)
			e.peerRequestQueue.Push(entry, l.Partner, receipt)
			work = true
		}
		l.lk.Unlock()
	}

	if work {
		e.signalNewWork()
	}
}

func (e *Engine) AddBlock(block blocks.Block) {
	e.lock.Lock()
	defer e.lock.Unlock()

	e.addBlock(block)
}

// TODO add contents of m.WantList() to my local wantlist? NB: could introduce
// race conditions where I send a message, but MessageSent gets handled after
// MessageReceived. The information in the local wantlist could become
// inconsistent. Would need to ensure that Sends and acknowledgement of the
// send happen atomically

func (e *Engine) MessageSent(p peer.ID, m bsmsg.BitSwapMessage) error {
	l := e.findOrCreate(p)
	l.lk.Lock()
	defer l.lk.Unlock()

	for _, block := range m.Blocks() {
		l.SentBytes(len(block.RawData()))
		l.wantList.Remove(block.Cid())
		e.peerRequestQueue.Remove(block.Cid(), p)
	}

	return nil
}

func (e *Engine) PeerConnected(p peer.ID) {
	e.lock.Lock()
	defer e.lock.Unlock()
	l, ok := e.ledgerMap[p]
	if !ok {
		l = newLedger(p)
		e.ledgerMap[p] = l
		if strings.Contains(p.String(), "Wyntbo") || strings.Contains(p.String(), "bskDM7") {
			color.Red(fmt.Sprintf("### Ledger reinitialised for %v (peerCo)", p.String()))
		}
	}
	l.lk.Lock()
	defer l.lk.Unlock()
	l.ref++
}

func (e *Engine) PeerDisconnected(p peer.ID) {
	e.lock.Lock()
	defer e.lock.Unlock()
	l, ok := e.ledgerMap[p]
	if !ok {
		return
	}
	l.lk.Lock()
	defer l.lk.Unlock()
	l.ref--
	if l.ref <= 0 {
		delete(e.ledgerMap, p)
		if strings.Contains(p.String(), "Wyntbo") || strings.Contains(p.String(), "bskDM7") {
			color.Red(fmt.Sprintf("### Ledger reinitialised for %v (peerDisco)", p.String()))
		}
	}
}

func (e *Engine) numBytesSentTo(p peer.ID) uint64 {
	// NB not threadsafe
	return e.findOrCreate(p).Accounting.BytesSent
}

func (e *Engine) numBytesReceivedFrom(p peer.ID) uint64 {
	// NB not threadsafe
	return e.findOrCreate(p).Accounting.BytesRecv
}

// ledger lazily instantiates a ledger
func (e *Engine) findOrCreate(p peer.ID) *ledger {
	e.lock.Lock()
	defer e.lock.Unlock()
	l, ok := e.ledgerMap[p]
	if !ok {
		l = newLedger(p)
		e.ledgerMap[p] = l
		if strings.Contains(p.String(), "Wyntbo") || strings.Contains(p.String(), "bskDM7") {
			color.Red(fmt.Sprintf("### Ledger reinitialised for %v (notFound)", p.String()))
		}
	}
	return l
}

func (e *Engine) signalNewWork() {
	// Signal task generation to restart (if stopped!)
	select {
	case e.workSignal <- struct{}{}:
	default:
	}
}
