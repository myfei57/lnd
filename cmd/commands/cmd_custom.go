package commands

import (
	"encoding/hex"
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

var sendCustomCommand = cli.Command{
	Name:     "sendcustom",
	Category: "Peers",
	Usage:    "Send a custom p2p wire message to a peer",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "peer",
		},
		cli.Uint64Flag{
			Name: "type",
		},
		cli.StringFlag{
			Name: "data",
		},
	},
	Action: actionDecorator(sendCustom),
}

func sendCustom(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	peer, err := hex.DecodeString(ctx.String("peer"))
	if err != nil {
		return err
	}

	msgType := ctx.Uint64("type")

	data, err := hex.DecodeString(ctx.String("data"))
	if err != nil {
		return err
	}

	resp, err := client.SendCustomMessage(
		ctxc, &lnrpc.SendCustomMessageRequest{
			Peer: peer,
			Type: uint32(msgType),
			Data: data,
		},
	)

	printRespJSON(resp)

	return err
}

var subscribeCustomCommand = cli.Command{
	Name:     "subscribecustom",
	Category: "Peers",
	Usage: "Subscribe to incoming custom p2p wire messages, optionally " +
		"filtered by source peer and message type",
	Flags: []cli.Flag{
		cli.StringSliceFlag{
			Name: "peer",
			Usage: "hex-encoded 33-byte compressed public key of " +
				"a peer to receive messages from (the peer " +
				"does not need to be connected). Can be set " +
				"multiple times; a match against any entry is " +
				"sufficient",
		},
		cli.Int64SliceFlag{
			Name: "type",
			Usage: "custom message type (32768 to 65535, or an " +
				"enabled protocol override type) to receive. " +
				"Can be set multiple times; a match against " +
				"any entry is sufficient",
		},
	},
	Action: actionDecorator(subscribeCustom),
}

func subscribeCustom(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.SubscribeCustomMessagesRequest{}

	for _, peerHex := range ctx.StringSlice("peer") {
		peer, err := hex.DecodeString(peerHex)
		if err != nil {
			return fmt.Errorf("could not hex decode peer %v: %w",
				peerHex, err)
		}

		req.PeerPubkeys = append(req.PeerPubkeys, peer)
	}

	for _, msgType := range ctx.Int64Slice("type") {
		if msgType < 0 || msgType > 65535 {
			return fmt.Errorf("invalid message type %d: must be "+
				"between 0 and 65535", msgType)
		}

		req.MessageTypes = append(
			req.MessageTypes, uint32(msgType),
		)
	}

	stream, err := client.SubscribeCustomMessages(ctxc, req)
	if err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}

		fmt.Printf("Received from peer %x: type=%d, data=%x\n",
			msg.Peer, msg.Type, msg.Data)
	}
}
