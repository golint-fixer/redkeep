package redkeep

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

const requeryDuration = 1 * time.Second

//TailAgent the worker that tails the database
type TailAgent struct {
	config    Configuration
	session   *mgo.Session
	tracker   Tracker
	startTime time.Time
}

//Query represents a mongodb oplog query
type Query interface {
	DB() string
	C() string
	OP() string
}

type oplogQuery struct {
	dataset        map[string]interface{}
	db, collection string
}

func (o oplogQuery) C() string {
	return o.collection
}
func (o oplogQuery) DB() string {
	return o.db
}

func (o oplogQuery) OP() string {
	if s, ok := o.dataset["op"].(string); ok {
		return s
	}

	return ""
}

//NewOplogQuery generates a new query object from the given dataset
func NewOplogQuery(dataset map[string]interface{}) (Query, error) {
	namespace, ok := dataset["ns"].(string)
	if namespace == "" || !ok {
		return nil, errors.New("namespace not given")
	}

	p := strings.Index(namespace, ".")
	if p == -1 {
		return nil, errors.New("Invalid namespace given, must contain dot")
	}

	triggerDB := namespace[:p]
	triggerCollection := namespace[p+1:]

	return oplogQuery{dataset: dataset, db: triggerDB, collection: triggerCollection}, nil
}

//mongoTimestamp has the capability to cast to a mongo timestamp
type mongoTimestamp struct {
	time.Time
}

//mongoTimestamp returns a valid mongoTimestamp
func (m mongoTimestamp) MongoTimestamp() bson.MongoTimestamp {
	var b [12]byte
	var result uint64
	binary.BigEndian.PutUint32(b[:4], uint32(m.Unix()))
	binary.Read(bytes.NewReader(b[:]), binary.BigEndian, &result)

	return bson.MongoTimestamp(result)
}

func analyzeResult(dataset map[string]interface{}, w []Watch, s *mgo.Session) {
	query, err := NewOplogQuery(dataset)
	if err != nil {
		log.Println(err)
		return
	}

	session := s.Copy()
	defer session.Close()

	t := NewChangeTracker(session)
	watches := w
	triggerDB := query.DB()
	triggerCollection := query.C()
	operationType := query.OP()
	namespace := fmt.Sprintf("%s.%s", triggerDB, triggerCollection)

	if command, ok := dataset["o"].(map[string]interface{}); ok {
		triggerID, _ := command["_id"].(bson.ObjectId)
		triggerRef := mgo.DBRef{
			Database:   triggerDB,
			Id:         triggerID,
			Collection: triggerCollection,
		}

		for _, w := range watches {
			switch operationType {
			case "i":
				if w.TargetCollection == namespace {
					t.HandleInsert(w, command, triggerRef)
				}
			case "u":
				if w.TargetCollection == namespace {
					triggerRef := mgo.DBRef{
						Collection: triggerCollection,
						Database:   triggerDB,
						Id:         dataset["o2"].(map[string]interface{})["_id"].(bson.ObjectId),
					}

					t.HandleInsert(w, command, triggerRef)
				}

				if w.TrackCollection == namespace {
					if selector, ok := dataset["o2"].(map[string]interface{}); ok {
						t.HandleUpdate(w, command, selector)
					}
				}
			case "d":
				if w.TrackCollection == namespace {
					if selector, ok := dataset["o2"].(map[string]interface{}); ok {
						t.HandleRemove(w, command, selector)
					}
				}
			case "c":
				//system commands. We do not care.
			default:
				log.Printf("unsupported operation %s.\n", operationType)
				return
			}
		}
	}
}

//getReference tries to create a reference from target
//returns true if valid, false otherwise
func getReference(target interface{}, originalDatabase string) (mgo.DBRef, bool) {
	id, okID := GetValue("$id", target).(bson.ObjectId)
	col, okRef := GetValue("$ref", target).(string)

	//database in references is an optional value
	db, okDb := GetValue("$db", target).(string)

	if !okDb {
		db = originalDatabase
	}

	return mgo.DBRef{Collection: col, Id: id, Database: db}, okID && okRef
}

//Tail will start an inifite look that tails the oplog
//as long as the channel does not get any input
//forceRescan (Default false) will update anything from the lowest oplog timestamp
//again. Can cause many redundant writes depending on your oplog size.
func (t TailAgent) Tail(quit chan bool, forceRescan bool) error {
	session := t.session.Copy()
	defer session.Close()

	oplogCollection := session.DB("local").C("oplog.rs")

	startTime := mongoTimestamp{t.startTime}
	if forceRescan {
		startTime = mongoTimestamp{time.Unix(0, 0)}
	}

	query := oplogCollection.Find(bson.M{"ts": bson.M{"$gt": startTime.MongoTimestamp()}})
	iter := query.LogReplay().Sort("$natural").Tail(requeryDuration)

	sessionCopy := session.Copy()
	var lastTimestamp bson.MongoTimestamp
	for {
		select {
		case <-quit:
			log.Println("Agent stopped.")
			return nil
		default:
		}

		var result map[string]interface{}

		for iter.Next(&result) {
			lastTimestamp = result["ts"].(bson.MongoTimestamp)

			// in order to avoid a race condition, each routine needs
			// copies from everything.
			copyResult := make(map[string]interface{})
			for k, v := range result {
				copyResult[k] = v
			}

			go analyzeResult(copyResult, t.config.Watches[:], sessionCopy)
		}

		if iter.Err() != nil {
			return iter.Close()
		}

		if iter.Timeout() {
			continue
		}

		query := oplogCollection.Find(bson.M{"ts": bson.M{"$gt": lastTimestamp}})
		iter = query.LogReplay().Sort("$natural").Tail(requeryDuration)
	}

	iter.Close()

	return errors.New("Tailable cursor ended unexpectedly")
}

func (t *TailAgent) connect() error {
	log.Println("Connecting to", t.config.Mongo.ConnectionURI)
	session, err := mgo.Dial(t.config.Mongo.ConnectionURI)

	if err != nil {
		return err
	}

	session.SetMode(mgo.Strong, true)
	t.session = session
	t.tracker = NewChangeTracker(t.session)

	log.Println("Connected.")
	return nil
}

//NewTailAgentWithStartDate will start
func NewTailAgentWithStartDate(c Configuration, startTime time.Time) (*TailAgent, error) {
	agent := &TailAgent{config: c, startTime: startTime}
	err := agent.connect()
	return agent, err
}

//NewTailAgent will generate a new tail agent
func NewTailAgent(c Configuration) (*TailAgent, error) {
	return NewTailAgentWithStartDate(c, time.Now())
}
