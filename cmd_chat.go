package main
				//채팅시 시작 순서는 command -> main -> cmd_chat 순서로 진행됨
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/grpc"

	"github.com/jroimartin/gocui"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/urfave/cli"
)

var chatCommand = cli.Command{						//명령어 지정하는부분임 다른건 잘 모르겠는데 저기 action부분이
	Name:      "chat",						//저 명령어가 실행됬을때 아래에 함수로 지정해두면 실행되는거같음
	Category:  "Chat",
	ArgsUsage: "recipient_pubkey",
	Usage:     "Use lnd as a p2p messenger application.",
	Action:    actionDecorator(chat),
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name:  "amt_msat",
			Usage: "payment amount per chat message",
			Value: 1000,
		},
	},
}

var byteOrder = binary.BigEndian

const (
	tlvMsgRecord    = 34349334						//?? 이해가 안되네 뒤에 바로 다른값으로 바꾸면서 이걸 왜넣는거야
	tlvSigRecord    = 34349337
	tlvSenderRecord = 34349339
	tlvTimeRecord   = 34349343

	// TODO: Reference lnd master constant when available.
	tlvKeySendRecord = 5482373484
)

type messageState uint8

const (
	statePending messageState = iota

	stateDelivered

	stateFailed
)

type chatLine struct {								//내가쓰는 채팅 한줄한줄이 구조체 덩어리인데 그 구조체의 구조임
	text      string
	sender    route.Vertex
	recipient *route.Vertex
	state     messageState
	fee       uint64
	timestamp time.Time
}

var (
	msgLines       []chatLine
	destination    *route.Vertex
	runningBalance map[route.Vertex]int64 = make(map[route.Vertex]int64)

	keyToAlias = make(map[route.Vertex]string)
	aliasToKey = make(map[string]route.Vertex)

	self route.Vertex
)

func initAliasMaps(conn *grpc.ClientConn) error {
	client := lnrpc.NewLightningClient(conn)

	graph, err := client.DescribeGraph(
		context.Background(),
		&lnrpc.ChannelGraphRequest{},
	)
	if err != nil {
		return err
	}

	aliasCount := make(map[string]int)
	for _, node := range graph.Nodes {
		alias := node.Alias
		aliasCount[alias]++
	}

	for _, node := range graph.Nodes {
		alias := node.Alias

		key, err := route.NewVertexFromStr(node.PubKey)
		if err != nil {
			return err
		}

		if aliasCount[alias] > 1 {
			alias += "-" + node.PubKey[:6]
		}

		aliasToKey[alias] = key
		aliasToKey[key.String()] = key

		keyToAlias[key] = alias
	}

	info, err := client.GetInfo(context.Background(), &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}

	self, err = route.NewVertexFromStr(info.IdentityPubkey)
	if err != nil {
		return err
	}

	return nil
}

func setDest(destStr string) {
	dest, err := route.NewVertexFromStr(destStr)
	if err == nil {
		destination = &dest
	}

	if dest, ok := aliasToKey[destStr]; ok {
		destination = &dest
	}
}

func chat(ctx *cli.Context) error {
	chatMsgAmt := int64(ctx.Uint64("amt_msat"))	//chatMsgAmt ->1000 으로 고정인데 ctx 더 파보면 나올듯 찾아볼것.

	conn := getClientConn(ctx, false)
	defer conn.Close()

	err := initAliasMaps(conn)
	if err != nil {
		return err
	}

	if ctx.NArg() != 0 {
		destStr := ctx.Args().First()
		setDest(destStr)
	}

	mainRpc := lnrpc.NewLightningClient(conn)
	client := routerrpc.NewRouterClient(conn)
	signClient := signrpc.NewSignerClient(conn)

	req := &lnrpc.InvoiceSubscription{}
	rpcCtx := context.Background()
	stream, err := mainRpc.SubscribeInvoices(rpcCtx, req)
	if err != nil {
		return err
	}

	g, err := gocui.NewGui(gocui.OutputNormal)
	if err != nil {
		log.Panicln(err)
	}
	defer g.Close()

	g.SetManagerFunc(layout)

	if err := g.SetKeybinding("", gocui.KeyCtrlC, gocui.ModNone, quit); err != nil {
		log.Panicln(err)
	}
		
	addMsg := func(line chatLine) int {				/*내가 보내는 채팅, 내가 받는채팅 한줄한줄 추가하는부분*/
		msgLines = append(msgLines, line)
		return len(msgLines) - 1
	}

	sendMessage := func(g *gocui.Gui, v *gocui.View) error {	//메세지 보내는곳임. 시작할때나 메세지 받을때는 작동안함
		if len(v.BufferLines()) == 0 {
			return nil
		}
		newMsg := v.BufferLines()[0]				//내가 보내는 메세지 내용, gocui/view.go에 bufferline함수가 정의 되어있는데
									// [0] 여기에 내가보내는 채팅 내용이 저장되어있음 뒤에는 뭐 00 35 46 22 이런식으로
									// 못알아보게 숫자들이있음.. 추후에 뭔지 알아볼것.

		v.Clear()
		if err := v.SetCursor(0, 0); err != nil {
			return err
		}
		if err := v.SetOrigin(0, 0); err != nil {
			return err
		}

		if newMsg[0] == '/' {					//맨처음 실행할때 chat (주소 or alias)를 입력하는데 이때 입력하지 않고 넘어가면
			destHex := newMsg[1:]				// /주소 or /alias를 입력해서 채팅을 할수있음 이때 사용하는 부분임
			setDest(destHex)

			updateView(g)

			return nil
		}

		if destination == nil {
			return nil
		}
				
		d := *destination
		msgIdx := addMsg(chatLine{				//log.Panicln(&d)로 찍어보면 내가보내는 상대방의 id주소나옴
			sender:    self,				//신기하네 self log.Panicln(self)찍으면 내 id주소 나옴
			text:      newMsg,
			recipient: &d,
		})

		err := updateView(g)					
		if err != nil {
			return err
		}

		payAmt := runningBalance[*destination]				//payAmt는 채팅할때 창에 보면 send to young1 balance : 0
		if payAmt < chatMsgAmt {					//이런식으로 되어있는데 상대방이 보내면 1000씩 올라 그때 값이 payAmt임
			payAmt = chatMsgAmt					//chatMsgAmt는 1000으로 고정인듯
		}
		if payAmt > 10*chatMsgAmt {
			payAmt = 10 * chatMsgAmt
		}

		var preimage lntypes.Preimage
		if _, err := rand.Read(preimage[:]); err != nil {
			return err
		}
		hash := preimage.Hash()						//hash함수는 lnd에서 찾아보니
										//return Hash(sha256.Sum256(p[:])) 이걸 리턴함
										//확인해보니까 이 값이 내가 채팅 보낼떄 상대방 lnd 커멘드 창에
										//settling htlc ~~ 여기에 뜨는 hash값이네

		// Message sending time stamp
		timestamp := time.Now().UnixNano()				//그 유닉스타임 그걸 nano초로 바꾼거래
		var timeBuffer [8]byte
		byteOrder.PutUint64(timeBuffer[:], uint64(timestamp))

		// Sign all data.						//유닉스 타임 거지같이나오네 ^^ 80 214 215 23 5 이딴식으로나옴
		signData, err := getSignData(					//아래에 getSignData 확인하는데 값이 그냥 터지네; 0 34 25 3 23 이딴식으로 나오네 저것도 
			self, *destination, timeBuffer[:], []byte(newMsg),
		)
		if err != nil {
			return err
		}

		signResp, err := signClient.SignMessage(context.Background(), &signrpc.SignMessageReq{
			Msg: signData,
			KeyLoc: &signrpc.KeyLocator{
				KeyFamily: int32(keychain.KeyFamilyNodeKey),
				KeyIndex:  0,
			},
		})
		if err != nil {
			return err
		}
		signature := signResp.Signature

		customRecords := map[uint64][]byte{
			tlvMsgRecord:     []byte(newMsg),
			tlvSenderRecord:  self[:],
			tlvTimeRecord:    timeBuffer[:],
			tlvSigRecord:     signature,
			tlvKeySendRecord: preimage[:],
		}

		req := routerrpc.SendPaymentRequest{
			PaymentHash:       hash[:],
			AmtMsat:           payAmt,
			FinalCltvDelta:    40,
			Dest:              destination[:],
			FeeLimitMsat:      chatMsgAmt * 10,
			TimeoutSeconds:    30,
			DestCustomRecords: customRecords,
		}

		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := client.SendPayment(ctx, &req)
			if err != nil {
				g.Update(func(g *gocui.Gui) error {
					return err
				})
				return
			}

			for {
				status, err := stream.Recv()
				if err != nil {
					break
				}

				switch status.State {
				case routerrpc.PaymentState_SUCCEEDED:
					msgLines[msgIdx].fee = uint64(status.Route.TotalFeesMsat)
					runningBalance[*destination] -= payAmt
					msgLines[msgIdx].state = stateDelivered
					g.Update(updateView)
					break

				case routerrpc.PaymentState_IN_FLIGHT:

				default:
					msgLines[msgIdx].state = stateFailed
					g.Update(updateView)
					break
				}
			}
		}()

		return nil
	}

	err = g.SetKeybinding("send", gocui.KeyEnter, gocui.ModNone, sendMessage)	//마지막 send라는 뷰에 엔터를 누르면 sendMessage
	if err != nil {									//라는 handler를? 이용해서 실행하는데 더 자세히 알아봐야 
		return err
	}

	go func() {
		returnErr := func(err error) {
			g.Update(func(g *gocui.Gui) error {
				return err
			})
		}

		for {
			invoice, err := stream.Recv()
			if err != nil {
				returnErr(err)
				return
			}

			if invoice.State != lnrpc.Invoice_SETTLED {
				continue
			}

			var customRecords map[uint64][]byte
			for _, htlc := range invoice.Htlcs {
				if htlc.State == lnrpc.InvoiceHTLCState_SETTLED {
					customRecords = htlc.CustomRecords
					break
				}
			}
			if customRecords == nil {
				continue
			}

			msg, ok := customRecords[tlvMsgRecord]
			if !ok {
				continue
			}

			signature, ok := customRecords[tlvSigRecord]
			if !ok {
				continue
			}

			timestampBytes, ok := customRecords[tlvTimeRecord]
			if !ok {
				continue
			}
			timestamp := time.Unix(
				0,
				int64(byteOrder.Uint64(timestampBytes)),
			)

			senderBytes, ok := customRecords[tlvSenderRecord]
			if !ok {
				continue
			}
			sender, err := route.NewVertexFromBytes(senderBytes)
			if err != nil {
				// Invalid sender pubkey
				continue
			}

			signData, err := getSignData(sender, self, timestampBytes, msg)
			if err != nil {
				returnErr(err)
				return
			}

			verifyResp, err := signClient.VerifyMessage(
				context.Background(),
				&signrpc.VerifyMessageReq{
					Msg:       signData,
					Signature: signature,
					Pubkey:    sender[:],
				})
			if err != nil {
				returnErr(err)
				return
			}

			if !verifyResp.Valid {
				continue
			}

			if destination == nil {
				destination = &sender
			}

			addMsg(chatLine{
				sender:    sender,
				text:      string(msg),
				timestamp: timestamp,
			})
			g.Update(updateView)

			amt := invoice.AmtPaid
			runningBalance[*destination] += amt
		}
	}()

	if err := g.MainLoop(); err != nil && err != gocui.ErrQuit {
		return err
	}

	return nil
}

func layout(g *gocui.Gui) error {
	g.Cursor = true

	maxX, maxY := g.Size()
	if v, err := g.SetView("messages", 0, 0, maxX-1, maxY-5); err != nil {
		if err != gocui.ErrUnknownView {
			return err
		}
		v.Title = " Messages "
	}

	if v, err := g.SetView("send", 0, maxY-4, maxX-1, maxY-1); err != nil {
		if _, err := g.SetCurrentView("send"); err != nil {
			return err
		}

		if err != gocui.ErrUnknownView {
			return err
		}

		v.Editable = true
	}

	updateView(g)

	return nil
}

func quit(g *gocui.Gui, v *gocui.View) error {
	return gocui.ErrQuit
}

func updateView(g *gocui.Gui) error {
	const (
		maxSenderLen = 16
	)

	sendView, _ := g.View("send")					
	if destination == nil {
		sendView.Title = " Set a destination by typing /pubkey "
	} else {
		alias := keyToAlias[*destination]
		sendView.Title = fmt.Sprintf(" Send to %v [balance: %v msat]",
			alias, runningBalance[*destination])
	}

	messagesView, _ := g.View("messages")

	messagesView.Clear()
	cols, rows := messagesView.Size()

	startLine := len(msgLines) - rows
	if startLine < 0 {
		startLine = 0
	}

	for _, line := range msgLines[startLine:] {
		text := line.text					//딱 여기가 채팅을 받았을때 view에 보여지는 부분임. 
		var r string						//그래서 초반에 채팅 보내는거 할떄 실수한게 여기에서 값을 추가하면
		if line.recipient != nil {				//내 화면에서만 추가된걸로 보임.
			r = keyToAlias[*line.recipient]
		} else {
			r = fmt.Sprintf("sent: %v",
				line.timestamp.Format(time.ANSIC))
		}

		text += fmt.Sprintf(" \x1b[34m(%v)\x1b[0m", r)

		var amtDisplay string
		if line.state == stateDelivered {
			amtDisplay = formatMsat(line.fee)
		}

		maxTextFieldLen := cols - len(amtDisplay) - maxSenderLen + 5
		maxTextLen := maxTextFieldLen
		if line.state != statePending {
			maxTextLen -= 2
		}
		if len(text) > maxTextLen {
			text = text[:maxTextLen-3] + "..."
		}
		paddingLen := maxTextFieldLen - len(text)
		switch line.state {
		case stateDelivered:
			text += " \x1b[34m✔️\x1b[0m"
			paddingLen -= 2
		case stateFailed:
			text += " \x1b[31m✘\x1b[0m"
			paddingLen -= 2
		}

		text += strings.Repeat(" ", paddingLen)

		senderAlias := keyToAlias[line.sender]
		if len(senderAlias) > maxSenderLen {
			senderAlias = senderAlias[:maxSenderLen]
		}
		fmt.Fprintf(messagesView, "%16v: %v \x1b[34m%v\x1b[0m",
			senderAlias,
			text, amtDisplay,
		)

		fmt.Fprintln(messagesView)
	}
	return nil
}

func formatMsat(msat uint64) string {
	wholeSats := msat / 1000
	msats := msat % 1000
	var msatsStr string
	if msats > 0 {
		msatsStr = fmt.Sprintf(".%03d", msats)
		msatsStr = strings.TrimRight(msatsStr, "0")
	}
	return fmt.Sprintf("[%d%-4s sat]",
		wholeSats, msatsStr,
	)
}

func getSignData(sender, recipient route.Vertex, timestamp []byte, msg []byte) ([]byte, error) {
	var signData bytes.Buffer

	// Write sender.
	if _, err := signData.Write(sender[:]); err != nil {
		return nil, err
	}

	// Write recipient.
	if _, err := signData.Write(recipient[:]); err != nil {
		return nil, err
	}

	// Write time.
	if _, err := signData.Write(timestamp); err != nil {
		return nil, err
	}

	// Write message.
	if _, err := signData.Write(msg); err != nil {
		return nil, err
	}

	return signData.Bytes(), nil
}
