package crawl

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	mrand "math/rand"
	"time"

	host "github.com/libp2p/go-libp2p-host"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	peer "github.com/libp2p/go-libp2p-peer"
	pstore "github.com/libp2p/go-libp2p-peerstore"
	swarm "github.com/libp2p/go-libp2p-swarm"
)

const WORKERS = 16

type Crawler struct {
	ctx context.Context
	h   host.Host
	dht *dht.IpfsDHT

	peers map[peer.ID]struct{}
	work  chan pstore.PeerInfo

	Discovered chan pstore.PeerInfo
}

func NewCrawler(ctx context.Context, h host.Host, dht *dht.IpfsDHT) *Crawler {
	c := &Crawler{ctx: ctx, h: h, dht: dht,
		peers:      make(map[peer.ID]struct{}),
		work:       make(chan pstore.PeerInfo, WORKERS),
		Discovered: make(chan pstore.PeerInfo, 256),
	}

	for i := 0; i < WORKERS; i++ {
		go c.worker()
	}

	return c
}

func (c *Crawler) Crawl() {
	anchor := make([]byte, 32)
	for {
		_, err := crand.Read(anchor)
		if err != nil {
			log.Fatal(err)
		}

		str := base64.RawStdEncoding.EncodeToString(anchor)
		c.crawlFromAnchor(str)

		select {
		case <-time.After(5 * time.Second):
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Crawler) crawlFromAnchor(key string) {
	// fmt.Printf("Crawling from anchor %s\n", key)

	ctx, cancel := context.WithTimeout(c.ctx, 60*time.Second)
	pch, err := c.dht.GetClosestPeers(ctx, key)

	if err != nil {
		log.Fatal(err)
	}

	var ps []peer.ID
	for p := range pch {
		ps = append(ps, p)
	}
	cancel()

	// fmt.Printf("Found %d peers\n", len(ps))
	for _, p := range ps {
		c.crawlPeer(p)
	}
}

func (c *Crawler) crawlPeer(p peer.ID) {
	_, ok := c.peers[p]
	if ok {
		return
	}

	// fmt.Printf("Crawling peer %s\n", p.Pretty())

	ctx, cancel := context.WithTimeout(c.ctx, 60*time.Second)
	pi, err := c.dht.FindPeer(ctx, p)
	cancel()

	if err != nil {
		// fmt.Printf("Peer not found %s: %s\n", p.Pretty(), err.Error())
		return
	}

	c.peers[p] = struct{}{}
	select {
	case c.work <- pi:
	case <-c.ctx.Done():
		return
	}

	ctx, cancel = context.WithTimeout(c.ctx, 60*time.Second)
	pch, err := c.dht.FindPeersConnectedToPeer(ctx, p)

	if err != nil {
		// fmt.Printf("Can't find peers connected to peer %s: %s\n", p.Pretty(), err.Error())
		cancel()
		return
	}

	var ps []peer.ID
	for pip := range pch {
		ps = append(ps, pip.ID)
	}
	cancel()

	// fmt.Printf("Peer %s is connected to %d peers\n", p.Pretty(), len(ps))

	for _, p := range ps {
		c.crawlPeer(p)
	}
}

func (c *Crawler) worker() {
	for {
		select {
		case pi, ok := <-c.work:
			if !ok {
				return
			}
			// add a bit of delay to avoid connection storms
			dt := mrand.Intn(60000)
			time.Sleep(time.Duration(dt) * time.Millisecond)
			c.tryConnect(pi)

		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Crawler) tryConnect(pi pstore.PeerInfo) {
	backoff := 0
	var ctx context.Context
	var cancel func()

again:
	// fmt.Printf("Connecting to %s (%d)\n", pi.ID.Pretty(), len(pi.Addrs))
	ctx, cancel = context.WithTimeout(c.ctx, 60*time.Second)

	err := c.h.Connect(ctx, pi)
	cancel()

	switch {
	case err == swarm.ErrDialBackoff:
		backoff++
		if backoff < 7 {
			dt := 1000 + mrand.Intn(backoff*10000)
			// fmt.Printf("Backing off dialing %s\n", pi.ID.Pretty())
			time.Sleep(time.Duration(dt) * time.Millisecond)
			goto again
		} else {
			// fmt.Printf("FAILED to connect to %s; giving up from dial backoff\n", pi.ID.Pretty())
		}
	case err != nil:
		// fmt.Printf("FAILED to connect to %s: %s", pi.ID.Pretty(), err.Error())
	default:
		// fmt.Printf("CONNECTED to %s", pi.ID.Pretty())

		c.Discovered <- pi

		conns := c.h.Network().ConnsToPeer(pi.ID)
		if len(conns) == 0 {
			fmt.Println("ERROR: supposedly connected, but no conns to peer", pi.ID.Pretty())
		}
	}
}
