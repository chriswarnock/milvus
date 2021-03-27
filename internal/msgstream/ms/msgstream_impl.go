package ms

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/zilliztech/milvus-distributed/internal/log"
	"github.com/zilliztech/milvus-distributed/internal/msgstream"
	"github.com/zilliztech/milvus-distributed/internal/msgstream/client"
	"github.com/zilliztech/milvus-distributed/internal/msgstream/util"
	"github.com/zilliztech/milvus-distributed/internal/proto/commonpb"
	"github.com/zilliztech/milvus-distributed/internal/proto/internalpb"
	"github.com/zilliztech/milvus-distributed/internal/util/trace"
	"go.uber.org/zap"
)

type MessageID = client.MessageID
type Client = client.Client
type Producer = client.Producer
type Consumer = client.Consumer
type TsMsg = msgstream.TsMsg
type MsgPack = msgstream.MsgPack
type MsgType = msgstream.MsgType
type UniqueID = msgstream.UniqueID
type BaseMsg = msgstream.BaseMsg
type Timestamp = msgstream.Timestamp
type IntPrimaryKey = msgstream.IntPrimaryKey
type TimeTickMsg = msgstream.TimeTickMsg
type QueryNodeStatsMsg = msgstream.QueryNodeStatsMsg
type RepackFunc = msgstream.RepackFunc
type UnmarshalDispatcher = msgstream.UnmarshalDispatcher

type msgStream struct {
	ctx              context.Context
	client           Client
	producers        map[string]Producer
	producerChannels []string
	consumers        map[string]Consumer
	consumerChannels []string
	repackFunc       RepackFunc
	unmarshal        UnmarshalDispatcher
	receiveBuf       chan *MsgPack
	wait             *sync.WaitGroup
	streamCancel     func()
	bufSize          int64
	producerLock     *sync.Mutex
	consumerLock     *sync.Mutex

	scMap *sync.Map
}

func NewMsgStream(ctx context.Context,
	receiveBufSize int64,
	bufSize int64,
	client Client,
	unmarshal UnmarshalDispatcher) (*msgStream, error) {

	streamCtx, streamCancel := context.WithCancel(ctx)
	producers := make(map[string]Producer)
	consumers := make(map[string]Consumer)
	producerChannels := make([]string, 0)
	consumerChannels := make([]string, 0)
	receiveBuf := make(chan *MsgPack, receiveBufSize)

	var err error
	if err != nil {
		defer streamCancel()
		log.Error("Set client failed, error", zap.Error(err))
		return nil, err
	}

	stream := &msgStream{
		ctx:              streamCtx,
		client:           client,
		producers:        producers,
		producerChannels: producerChannels,
		consumers:        consumers,
		consumerChannels: consumerChannels,
		unmarshal:        unmarshal,
		bufSize:          bufSize,
		receiveBuf:       receiveBuf,
		streamCancel:     streamCancel,
		producerLock:     &sync.Mutex{},
		consumerLock:     &sync.Mutex{},
		wait:             &sync.WaitGroup{},
		scMap:            &sync.Map{},
	}

	return stream, nil
}

func (ms *msgStream) AsProducer(channels []string) {
	for _, channel := range channels {
		fn := func() error {
			pp, err := ms.client.CreateProducer(client.ProducerOptions{Topic: channel})
			if err != nil {
				return err
			}
			if pp == nil {
				return errors.New("Producer is nil")
			}

			ms.producerLock.Lock()
			ms.producers[channel] = pp
			ms.producerChannels = append(ms.producerChannels, channel)
			ms.producerLock.Unlock()
			return nil
		}
		err := util.Retry(20, time.Millisecond*200, fn)
		if err != nil {
			errMsg := "Failed to create producer " + channel + ", error = " + err.Error()
			panic(errMsg)
		}
	}
}

func (ms *msgStream) AsConsumer(channels []string,
	subName string) {
	for _, channel := range channels {
		if _, ok := ms.consumers[channel]; ok {
			continue
		}
		fn := func() error {
			receiveChannel := make(chan client.ConsumerMessage, ms.bufSize)
			pc, err := ms.client.Subscribe(client.ConsumerOptions{
				Topic:                       channel,
				SubscriptionName:            subName,
				Type:                        client.KeyShared,
				SubscriptionInitialPosition: client.SubscriptionPositionEarliest,
				MessageChannel:              receiveChannel,
			})
			if err != nil {
				return err
			}
			if pc == nil {
				return errors.New("Consumer is nil")
			}

			ms.consumers[channel] = pc
			ms.consumerChannels = append(ms.consumerChannels, channel)
			ms.wait.Add(1)
			go ms.receiveMsg(pc)
			return nil
		}
		err := util.Retry(20, time.Millisecond*200, fn)
		if err != nil {
			errMsg := "Failed to create consumer " + channel + ", error = " + err.Error()
			panic(errMsg)
		}
	}
}

func (ms *msgStream) SetRepackFunc(repackFunc RepackFunc) {
	ms.repackFunc = repackFunc
}

func (ms *msgStream) Start() {
}

func (ms *msgStream) Close() {
	ms.streamCancel()
	ms.wait.Wait()

	for _, producer := range ms.producers {
		if producer != nil {
			producer.Close()
		}
	}
	for _, consumer := range ms.consumers {
		if consumer != nil {
			consumer.Close()
		}
	}
	if ms.client != nil {
		ms.client.Close()
	}
}

func (ms *msgStream) Produce(msgPack *MsgPack) error {
	tsMsgs := msgPack.Msgs
	if len(tsMsgs) <= 0 {
		log.Debug("Warning: Receive empty msgPack")
		return nil
	}
	if len(ms.producers) <= 0 {
		return errors.New("nil producer in msg stream")
	}
	reBucketValues := make([][]int32, len(tsMsgs))
	for idx, tsMsg := range tsMsgs {
		hashValues := tsMsg.HashKeys()
		bucketValues := make([]int32, len(hashValues))
		for index, hashValue := range hashValues {
			if tsMsg.Type() == commonpb.MsgType_SearchResult {
				searchResult := tsMsg.(*msgstream.SearchResultMsg)
				channelID := searchResult.ResultChannelID
				channelIDInt, _ := strconv.ParseInt(channelID, 10, 64)
				if channelIDInt >= int64(len(ms.producers)) {
					return errors.New("Failed to produce msg to unKnow channel")
				}
				bucketValues[index] = int32(hashValue % uint32(len(ms.producers)))
				continue
			}
			bucketValues[index] = int32(hashValue % uint32(len(ms.producers)))
		}
		reBucketValues[idx] = bucketValues
	}

	var result map[int32]*MsgPack
	var err error
	if ms.repackFunc != nil {
		result, err = ms.repackFunc(tsMsgs, reBucketValues)
	} else {
		msgType := (tsMsgs[0]).Type()
		switch msgType {
		case commonpb.MsgType_Insert:
			result, err = util.InsertRepackFunc(tsMsgs, reBucketValues)
		case commonpb.MsgType_Delete:
			result, err = util.DeleteRepackFunc(tsMsgs, reBucketValues)
		default:
			result, err = util.DefaultRepackFunc(tsMsgs, reBucketValues)
		}
	}
	if err != nil {
		return err
	}
	for k, v := range result {
		channel := ms.producerChannels[k]
		for i := 0; i < len(v.Msgs); i++ {
			mb, err := v.Msgs[i].Marshal(v.Msgs[i])
			if err != nil {
				return err
			}

			m, err := msgstream.ConvertToByteArray(mb)
			if err != nil {
				return err
			}

			msg := &client.ProducerMessage{Payload: m, Properties: map[string]string{}}

			sp, spanCtx := trace.MsgSpanFromCtx(v.Msgs[i].TraceCtx(), v.Msgs[i])
			trace.InjectContextToPulsarMsgProperties(sp.Context(), msg.Properties)

			if err := ms.producers[channel].Send(
				spanCtx,
				msg,
			); err != nil {
				trace.LogError(sp, err)
				sp.Finish()
				return err
			}
			sp.Finish()
		}
	}
	return nil
}

func (ms *msgStream) Broadcast(msgPack *MsgPack) error {
	for _, v := range msgPack.Msgs {
		mb, err := v.Marshal(v)
		if err != nil {
			return err
		}

		m, err := msgstream.ConvertToByteArray(mb)
		if err != nil {
			return err
		}

		msg := &client.ProducerMessage{Payload: m, Properties: map[string]string{}}

		sp, spanCtx := trace.MsgSpanFromCtx(v.TraceCtx(), v)
		trace.InjectContextToPulsarMsgProperties(sp.Context(), msg.Properties)

		ms.producerLock.Lock()
		for _, producer := range ms.producers {
			if err := producer.Send(
				spanCtx,
				msg,
			); err != nil {
				trace.LogError(sp, err)
				sp.Finish()
				return err
			}
		}
		ms.producerLock.Unlock()
		sp.Finish()
	}
	return nil
}

func (ms *msgStream) Consume() *MsgPack {
	for {
		select {
		case cm, ok := <-ms.receiveBuf:
			if !ok {
				log.Debug("buf chan closed")
				return nil
			}
			return cm
		case <-ms.ctx.Done():
			//log.Debug("context closed")
			return nil
		}
	}
}

func (ms *msgStream) receiveMsg(consumer Consumer) {
	defer ms.wait.Done()

	for {
		select {
		case <-ms.ctx.Done():
			return
		case msg, ok := <-consumer.Chan():
			if !ok {
				return
			}
			consumer.Ack(msg)
			headerMsg := commonpb.MsgHeader{}
			err := proto.Unmarshal(msg.Payload(), &headerMsg)
			if err != nil {
				log.Error("Failed to unmarshal message header", zap.Error(err))
				continue
			}
			tsMsg, err := ms.unmarshal.Unmarshal(msg.Payload(), headerMsg.Base.MsgType)
			if err != nil {
				log.Error("Failed to unmarshal tsMsg", zap.Error(err))
				continue
			}

			tsMsg.SetPosition(&msgstream.MsgPosition{
				ChannelName: filepath.Base(msg.Topic()),
				//FIXME
				MsgID: msg.ID().Serialize(),
			})

			sp, ok := trace.ExtractFromPulsarMsgProperties(tsMsg, msg.Properties())
			if ok {
				ms.scMap.Store(tsMsg.ID(), sp.Context())
			}

			msgPack := MsgPack{Msgs: []TsMsg{tsMsg}}
			ms.receiveBuf <- &msgPack

			sp.Finish()
		}
	}
}

func (ms *msgStream) Chan() <-chan *MsgPack {
	return ms.receiveBuf
}

func (ms *msgStream) Seek(mp *internalpb.MsgPosition) error {
	if _, ok := ms.consumers[mp.ChannelName]; ok {
		consumer := ms.consumers[mp.ChannelName]
		messageID, err := ms.client.BytesToMsgID(mp.MsgID)
		if err != nil {
			return err
		}
		err = consumer.Seek(messageID)
		if err != nil {
			return err
		}
		return nil
	}

	return errors.New("msgStream seek fail")
}

type TtMsgStream struct {
	msgStream
	unsolvedBuf     map[Consumer][]TsMsg
	msgPositions    map[Consumer]*internalpb.MsgPosition
	unsolvedMutex   *sync.Mutex
	lastTimeStamp   Timestamp
	syncConsumer    chan int
	stopConsumeChan map[Consumer]chan bool
}

func NewTtMsgStream(ctx context.Context,
	receiveBufSize int64,
	bufSize int64,
	client Client,
	unmarshal UnmarshalDispatcher) (*TtMsgStream, error) {
	msgStream, err := NewMsgStream(ctx, receiveBufSize, bufSize, client, unmarshal)
	if err != nil {
		return nil, err
	}
	unsolvedBuf := make(map[Consumer][]TsMsg)
	stopChannel := make(map[Consumer]chan bool)
	msgPositions := make(map[Consumer]*internalpb.MsgPosition)
	syncConsumer := make(chan int, 1)

	return &TtMsgStream{
		msgStream:       *msgStream,
		unsolvedBuf:     unsolvedBuf,
		msgPositions:    msgPositions,
		unsolvedMutex:   &sync.Mutex{},
		syncConsumer:    syncConsumer,
		stopConsumeChan: stopChannel,
	}, nil
}

func (ms *TtMsgStream) addConsumer(consumer Consumer, channel string) {
	if len(ms.consumers) == 0 {
		ms.syncConsumer <- 1
	}
	ms.consumers[channel] = consumer
	ms.unsolvedBuf[consumer] = make([]TsMsg, 0)
	ms.consumerChannels = append(ms.consumerChannels, channel)
	ms.msgPositions[consumer] = &internalpb.MsgPosition{
		ChannelName: channel,
		MsgID:       make([]byte, 0),
		Timestamp:   ms.lastTimeStamp,
	}
	stopConsumeChan := make(chan bool)
	ms.stopConsumeChan[consumer] = stopConsumeChan
}

func (ms *TtMsgStream) AsConsumer(channels []string,
	subName string) {
	for _, channel := range channels {
		if _, ok := ms.consumers[channel]; ok {
			continue
		}
		fn := func() error {
			receiveChannel := make(chan client.ConsumerMessage, ms.bufSize)
			pc, err := ms.client.Subscribe(client.ConsumerOptions{
				Topic:                       channel,
				SubscriptionName:            subName,
				Type:                        client.KeyShared,
				SubscriptionInitialPosition: client.SubscriptionPositionEarliest,
				MessageChannel:              receiveChannel,
			})
			if err != nil {
				return err
			}
			if pc == nil {
				return errors.New("Consumer is nil")
			}

			ms.consumerLock.Lock()
			ms.addConsumer(pc, channel)
			ms.consumerLock.Unlock()
			return nil
		}
		err := util.Retry(10, time.Millisecond*200, fn)
		if err != nil {
			errMsg := "Failed to create consumer " + channel + ", error = " + err.Error()
			panic(errMsg)
		}
	}
}

func (ms *TtMsgStream) Start() {
	if ms.consumers != nil {
		ms.wait.Add(1)
		go ms.bufMsgPackToChannel()
	}
}

func (ms *TtMsgStream) Close() {
	ms.streamCancel()
	close(ms.syncConsumer)
	ms.wait.Wait()

	for _, producer := range ms.producers {
		if producer != nil {
			producer.Close()
		}
	}
	for _, consumer := range ms.consumers {
		if consumer != nil {
			consumer.Close()
		}
	}
	if ms.client != nil {
		ms.client.Close()
	}
}

func (ms *TtMsgStream) bufMsgPackToChannel() {
	defer ms.wait.Done()
	ms.unsolvedBuf = make(map[Consumer][]TsMsg)
	isChannelReady := make(map[Consumer]bool)
	eofMsgTimeStamp := make(map[Consumer]Timestamp)

	if _, ok := <-ms.syncConsumer; !ok {
		log.Debug("consumer closed!")
		return
	}

	for {
		select {
		case <-ms.ctx.Done():
			return
		default:
			wg := sync.WaitGroup{}
			findMapMutex := sync.RWMutex{}
			ms.consumerLock.Lock()
			for _, consumer := range ms.consumers {
				if isChannelReady[consumer] {
					continue
				}
				wg.Add(1)
				go ms.findTimeTick(consumer, eofMsgTimeStamp, &wg, &findMapMutex)
			}
			ms.consumerLock.Unlock()
			wg.Wait()
			timeStamp, ok := checkTimeTickMsg(eofMsgTimeStamp, isChannelReady, &findMapMutex)
			if !ok || timeStamp <= ms.lastTimeStamp {
				//log.Printf("All timeTick's timestamps are inconsistent")
				continue
			}
			timeTickBuf := make([]TsMsg, 0)
			startMsgPosition := make([]*internalpb.MsgPosition, 0)
			endMsgPositions := make([]*internalpb.MsgPosition, 0)
			ms.unsolvedMutex.Lock()
			for consumer, msgs := range ms.unsolvedBuf {
				if len(msgs) == 0 {
					continue
				}
				tempBuffer := make([]TsMsg, 0)
				var timeTickMsg TsMsg
				for _, v := range msgs {
					if v.Type() == commonpb.MsgType_TimeTick {
						timeTickMsg = v
						continue
					}
					if v.EndTs() <= timeStamp {
						timeTickBuf = append(timeTickBuf, v)
					} else {
						tempBuffer = append(tempBuffer, v)
					}
				}
				ms.unsolvedBuf[consumer] = tempBuffer

				startMsgPosition = append(startMsgPosition, ms.msgPositions[consumer])
				var newPos *internalpb.MsgPosition
				if len(tempBuffer) > 0 {
					newPos = &internalpb.MsgPosition{
						ChannelName: tempBuffer[0].Position().ChannelName,
						MsgID:       tempBuffer[0].Position().MsgID,
						Timestamp:   timeStamp,
					}
					endMsgPositions = append(endMsgPositions, newPos)
				} else {
					newPos = &internalpb.MsgPosition{
						ChannelName: timeTickMsg.Position().ChannelName,
						MsgID:       timeTickMsg.Position().MsgID,
						Timestamp:   timeStamp,
					}
					endMsgPositions = append(endMsgPositions, newPos)
				}
				ms.msgPositions[consumer] = newPos
			}
			ms.unsolvedMutex.Unlock()

			msgPack := MsgPack{
				BeginTs:        ms.lastTimeStamp,
				EndTs:          timeStamp,
				Msgs:           timeTickBuf,
				StartPositions: startMsgPosition,
				EndPositions:   endMsgPositions,
			}

			ms.receiveBuf <- &msgPack
			ms.lastTimeStamp = timeStamp
		}
	}
}

func (ms *TtMsgStream) findTimeTick(consumer Consumer,
	eofMsgMap map[Consumer]Timestamp,
	wg *sync.WaitGroup,
	findMapMutex *sync.RWMutex) {
	defer wg.Done()
	for {
		select {
		case <-ms.ctx.Done():
			return
		case msg, ok := <-consumer.Chan():
			if !ok {
				log.Debug("consumer closed!")
				return
			}
			consumer.Ack(msg)

			headerMsg := commonpb.MsgHeader{}
			err := proto.Unmarshal(msg.Payload(), &headerMsg)
			if err != nil {
				log.Error("Failed to unmarshal message header", zap.Error(err))
				continue
			}
			tsMsg, err := ms.unmarshal.Unmarshal(msg.Payload(), headerMsg.Base.MsgType)
			if err != nil {
				log.Error("Failed to unmarshal tsMsg", zap.Error(err))
				continue
			}

			// set msg info to tsMsg
			tsMsg.SetPosition(&msgstream.MsgPosition{
				ChannelName: filepath.Base(msg.Topic()),
				MsgID:       msg.ID().Serialize(),
			})

			sp, ok := trace.ExtractFromPulsarMsgProperties(tsMsg, msg.Properties())
			if ok {
				ms.scMap.Store(tsMsg.ID(), sp.Context())
			}

			ms.unsolvedMutex.Lock()
			ms.unsolvedBuf[consumer] = append(ms.unsolvedBuf[consumer], tsMsg)
			ms.unsolvedMutex.Unlock()

			if headerMsg.Base.MsgType == commonpb.MsgType_TimeTick {
				findMapMutex.Lock()
				eofMsgMap[consumer] = tsMsg.(*TimeTickMsg).Base.Timestamp
				findMapMutex.Unlock()
				sp.Finish()
				return
			}
			sp.Finish()
		case <-ms.stopConsumeChan[consumer]:
			return
		}
	}
}

func checkTimeTickMsg(msg map[Consumer]Timestamp,
	isChannelReady map[Consumer]bool,
	mu *sync.RWMutex) (Timestamp, bool) {
	checkMap := make(map[Timestamp]int)
	var maxTime Timestamp = 0
	for _, v := range msg {
		checkMap[v]++
		if v > maxTime {
			maxTime = v
		}
	}
	if len(checkMap) <= 1 {
		for consumer := range msg {
			isChannelReady[consumer] = false
		}
		return maxTime, true
	}
	for consumer := range msg {
		mu.RLock()
		v := msg[consumer]
		mu.RUnlock()
		if v != maxTime {
			isChannelReady[consumer] = false
		} else {
			isChannelReady[consumer] = true
		}
	}

	return 0, false
}

func (ms *TtMsgStream) Seek(mp *internalpb.MsgPosition) error {
	if len(mp.MsgID) == 0 {
		return errors.New("when msgID's length equal to 0, please use AsConsumer interface")
	}
	var consumer Consumer
	var err error
	var hasWatched bool
	seekChannel := mp.ChannelName
	subName := mp.MsgGroup
	ms.consumerLock.Lock()
	defer ms.consumerLock.Unlock()
	consumer, hasWatched = ms.consumers[seekChannel]

	if hasWatched {
		return errors.New("the channel should has been subscribed")
	}

	receiveChannel := make(chan client.ConsumerMessage, ms.bufSize)
	consumer, err = ms.client.Subscribe(client.ConsumerOptions{
		Topic:                       seekChannel,
		SubscriptionName:            subName,
		SubscriptionInitialPosition: client.SubscriptionPositionEarliest,
		Type:                        client.KeyShared,
		MessageChannel:              receiveChannel,
	})
	if err != nil {
		return err
	}
	if consumer == nil {
		return errors.New("Consumer is nil")
	}

	seekMsgID, err := ms.client.BytesToMsgID(mp.MsgID)
	if err != nil {
		return err
	}
	consumer.Seek(seekMsgID)
	ms.addConsumer(consumer, seekChannel)

	//TODO: May cause problem
	if len(consumer.Chan()) == 0 {
		return nil
	}

	for {
		select {
		case <-ms.ctx.Done():
			return nil
		case msg, ok := <-consumer.Chan():
			if !ok {
				return errors.New("consumer closed")
			}
			consumer.Ack(msg)

			headerMsg := commonpb.MsgHeader{}
			err := proto.Unmarshal(msg.Payload(), &headerMsg)
			if err != nil {
				log.Error("Failed to unmarshal message header", zap.Error(err))
			}
			tsMsg, err := ms.unmarshal.Unmarshal(msg.Payload(), headerMsg.Base.MsgType)
			if err != nil {
				log.Error("Failed to unmarshal tsMsg", zap.Error(err))
			}
			if tsMsg.Type() == commonpb.MsgType_TimeTick {
				if tsMsg.BeginTs() >= mp.Timestamp {
					return nil
				}
				continue
			}
			if tsMsg.BeginTs() > mp.Timestamp {
				tsMsg.SetPosition(&msgstream.MsgPosition{
					ChannelName: filepath.Base(msg.Topic()),
					MsgID:       msg.ID().Serialize(),
				})
				ms.unsolvedBuf[consumer] = append(ms.unsolvedBuf[consumer], tsMsg)
			}
		}
	}
}

//TODO test InMemMsgStream
/*
type InMemMsgStream struct {
	buffer chan *MsgPack
}

func (ms *InMemMsgStream) Start() {}
func (ms *InMemMsgStream) Close() {}

func (ms *InMemMsgStream) ProduceOne(msg TsMsg) error {
	msgPack := MsgPack{}
	msgPack.BeginTs = msg.BeginTs()
	msgPack.EndTs = msg.EndTs()
	msgPack.Msgs = append(msgPack.Msgs, msg)
	buffer <- &msgPack
	return nil
}

func (ms *InMemMsgStream) Produce(msgPack *MsgPack) error {
	buffer <- msgPack
	return nil
}

func (ms *InMemMsgStream) Broadcast(msgPack *MsgPack) error {
	return ms.Produce(msgPack)
}

func (ms *InMemMsgStream) Consume() *MsgPack {
	select {
	case msgPack := <-ms.buffer:
		return msgPack
	}
}

func (ms *InMemMsgStream) Chan() <- chan *MsgPack {
	return buffer
}
*/