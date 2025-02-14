package eth

import (
	"context"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

type HeadSignal struct {
	Parent BlockID
	Self   BlockID
}

// HeadSignalFn is used as callback function to accept head-signals
type HeadSignalFn func(sig HeadSignal)

// WatchHeadChanges wraps a new-head subscription from NewHeadSource to feed the given Tracker
func WatchHeadChanges(ctx context.Context, src NewHeadSource, fn HeadSignalFn) (ethereum.Subscription, error) {
	headChanges := make(chan *types.Header, 10)
	sub, err := src.SubscribeNewHead(ctx, headChanges)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case header := <-headChanges:
				hash := header.Hash()
				height := header.Number.Uint64()
				self := BlockID{Hash: hash, Number: height}
				parent := BlockID{}
				if height > 0 {
					parent = BlockID{Hash: header.ParentHash, Number: height - 1}
				}
				fn(HeadSignal{Parent: parent, Self: self})
			case err := <-sub.Err():
				return err
			case <-ctx.Done():
				return ctx.Err()
			case <-quit:
				return nil
			}
		}
	}), nil
}
