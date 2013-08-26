package copycat

import (
	"bytes"
	"encoding/gob"
	"log"
	"net/mail"
	"sync"

	"code.google.com/p/go-imap/go1/imap"
	"github.com/bradfitz/gomemcache/memcache"
)

// searchAndStore will check check if each message in the source inbox
// exists in the destinations. If it doesn't exist in a destination, the message info will
// be pulled and stored into the destination.
func SearchAndStore(src InboxInfo, dsts []InboxInfo) (err error) {
	var cmd *imap.Command
	cmd, err = GetAllMessages(src)
	if err != nil {
		log.Printf("Unable to get all messages!")
		return
	}

	// setup message fetchers to pull from the source/memcache
	var fetchers sync.WaitGroup
	fetchRequests := make(chan fetchRequest)
	for j := 0; j < MaxImapConns; j++ {
		fetchers.Add(1)
		go fetchEmails(src, fetchRequests, &fetchers)
	}

	// setup storers for each destination
	var storers sync.WaitGroup
	var dstsStoreRequests []chan WorkRequest
	for _, dst := range dsts {
		storeRequests := make(chan WorkRequest)
		for i := 0; i < MaxImapConns; i++ {
			storers.Add(1)
			go checkAndStoreMessages(dst, storeRequests, fetchRequests, &storers)
		}

		dstsStoreRequests = append(dstsStoreRequests, storeRequests)
	}

	// build the requests and send them
	var rsp *imap.Response
	for _, rsp = range cmd.Data {
		header := imap.AsBytes(rsp.MessageInfo().Attrs["RFC822.HEADER"])
		if msg, _ := mail.ReadMessage(bytes.NewReader(header)); msg != nil {
			header := "Message-Id"
			value := msg.Header.Get(header)

			// create the store request and pass it to each dst's storers
			storeRequest := WorkRequest{Value: value, Header: header, UID: rsp.MessageInfo().UID}
			for _, storeRequests := range dstsStoreRequests {
				storeRequests <- storeRequest
			}
		}
	}

	// after everything is on the channel, close them...
	for _, storeRequests := range dstsStoreRequests {
		close(storeRequests)
	}
	// ... and wait for our workers to finish up.
	storers.Wait()

	// once the storers are complete we can close the fetch channel
	close(fetchRequests)
	// and then wait for the fetchers close connections
	fetchers.Wait()

	log.Printf("search and store processes complete")
	return nil
}

func checkAndStoreMessages(dst InboxInfo, storeRequests chan WorkRequest, fetchRequests chan fetchRequest, wg *sync.WaitGroup) {
	defer wg.Done()

	dstConn, err := GetConnection(dst, false)
	if err != nil {
		log.Printf("Unable to connect to destination: %s", err.Error())
		return
	}
	defer dstConn.Close(true)

	for request := range storeRequests {
		log.Printf("checking and storing (%s)", request.Value)

		// search for in dst
		cmd, err := imap.Wait(dstConn.UIDSearch([]imap.Field{"HEADER", request.Header, request.Value}))
		if err != nil {
			log.Printf("Unable to search for message (%s): %s", request.Value, err.Error())
			continue
		}

		results := cmd.Data[0].SearchResults()
		// if not found, PULL from SRC and STORE in DST
		if len(results) == 0 {

			// build and send fetch request
			response := make(chan imap.FieldMap)
			fr := fetchRequest{MessageId: request.Value, UID: request.UID, Response: response}
			fetchRequests <- fr

			// grab response from fetchers
			attrs := <-response
			if len(attrs) == 0 {
				log.Printf("No data found in message fetch request (%s)", request.Value)
				continue
			}

			msgDate := imap.AsDateTime(attrs["INTERNALDATE"])
			_, err = imap.Wait(dstConn.Append("INBOX", imap.NewFlagSet("UnSeen"), &msgDate, imap.NewLiteral(imap.AsBytes(attrs["BODY[]"]))))
			if err != nil {
				log.Printf("Problems removing message from dst: %s", err.Error())
				continue
			}

		}
	}
	log.Print("storer complete!")
	return
}

type fetchRequest struct {
	MessageId string
	UID       uint32
	Response  chan imap.FieldMap
}

// fetchEmails will sit and wait for fetchRequests from the destination workers. Once the
// requests channel is closed, this will finish up work and notify the waitgroup it is done.
func fetchEmails(src InboxInfo, requests chan fetchRequest, wg *sync.WaitGroup) {
	defer wg.Done()

	//connect to src imap
	conn, err := GetConnection(src, true)
	if err != nil {
		log.Printf("Unable to connect to source inbox: %s", err.Error())
		return
	}
	// connect to memcached
	cache := memcache.New(MemcacheServer)

	for request := range requests {
		// check if the message body is in memcached
		if msgBytes, err := cache.Get(request.MessageId); err != nil {

			var msgFields imap.FieldMap
			err := deserialize(msgBytes.Value, &msgFields)
			if err != nil {
				log.Printf("Problems deserializing memcache value: %s. Pulling message from src", err.Error())
				msgFields = imap.FieldMap{}
			}

			// if its there, respond with it
			if len(msgFields) > 0 {
				request.Response <- msgFields
				continue
			}
		}

		// if its not in the cache, fetch from the src and respond
		srcSeq, _ := imap.NewSeqSet("")
		srcSeq.AddNum(request.UID)
		cmd, err := imap.Wait(conn.UIDFetch(srcSeq, "INTERNALDATE", "BODY[]"))
		if err != nil {
			log.Printf("Unable to fetch message (%s) from src: %s", request.MessageId, err.Error())
			continue
		}

		if len(cmd.Data) == 0 {
			log.Printf("Unable to fetch message (%s) from src: NO DATA", request.MessageId)
			continue
		}

		msgFields := cmd.Data[0].MessageInfo().Attrs
		request.Response <- msgFields

		// store it in memcached if we had to fetch it
		// gobify
		msgGob, err := serialize(msgFields)
		if err != nil {
			log.Printf("Unable to serialize message (%s): %s", request.MessageId, err.Error())
			continue
		}

		cacheItem := memcache.Item{Key: request.MessageId, Value: msgGob}
		err = cache.Add(&cacheItem)
		if err != nil {
			log.Printf("Unable to add message (%s) to cache: %s", request.MessageId, err.Error())
		}
	}
}

// Serialize encodes a value using gob.
func serialize(src interface{}) ([]byte, error) {
	buf := new(bytes.Buffer)
	enc := gob.NewEncoder(buf)
	err := enc.Encode(src)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Deserialize decodes a value using gob.
func deserialize(src []byte, dst interface{}) error {
	buf := bytes.NewBuffer(src)
	dec := gob.NewDecoder(buf)
	err := dec.Decode(dst)
	if err != nil {
		return err
	}
	return nil
}
