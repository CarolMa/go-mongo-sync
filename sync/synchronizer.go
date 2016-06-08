/*
   MongoDB synchronizer implemention.
*/
package sync

import (
	"errors"
	"log"
	"runtime"
	"strings"
	"time"

	"go-mongo-sync/utils"

	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

type Synchronizer struct {
	config     Config
	srcSession *mgo.Session
	dstSession *mgo.Session
	optime     bson.MongoTimestamp // int64
}

// NewSynchronizer
//   - connect
//   - get optime
func NewSynchronizer(config Config) *Synchronizer {
	p := new(Synchronizer)
	p.config = config
	if s, err := mgo.DialWithTimeout(p.config.From, time.Second*3); err == nil {
		p.srcSession = s
		p.srcSession.SetSocketTimeout(0)
		p.srcSession.SetSyncTimeout(0)
		p.srcSession.SetMode(mgo.Strong, false)
		p.srcSession.SetCursorTimeout(0)
		log.Printf("connected to %s\n", p.config.From)
	} else {
		log.Println(err, p.config.From)
		return nil
	}
	if s, err := mgo.DialWithTimeout(p.config.To, time.Second*3); err == nil {
		p.dstSession = s
		p.dstSession.SetSocketTimeout(0)
		p.dstSession.SetSyncTimeout(0)
		p.dstSession.SetSafe(&mgo.Safe{W: 1})
		p.dstSession.SetMode(mgo.Eventual, false)
		log.Printf("connected to %s\n", p.config.To)
	} else {
		log.Println(err, p.config.To)
		return nil
	}
	if optime, err := utils.GetOptime(p.srcSession); err == nil {
		p.optime = optime
	} else {
		log.Println(err)
		return nil
	}
	log.Printf("optime: %v %v\n", utils.GetTimestampFromOptime(p.optime), utils.GetTimeFromOptime(p.optime))
	return p
}

func (p *Synchronizer) Run() error {
	if !p.config.OplogOnly {
		if err := p.initialSync(); err != nil {
			return err
		}
	}
	if err := p.oplogSync(); err != nil {
		return err
	}
	return nil
}

func (p *Synchronizer) initialSync() error {
	p.syncDatabases()
	return nil
}

func (p *Synchronizer) oplogSync() error {
	// oplog replayer runs background
	replayer := NewOplogReplayer(p.config.From, p.config.To, p.optime)
	if replayer == nil {
		return errors.New("NewOplogReplayer failed")
	}
	replayer.Run()
	return nil
}

func (p *Synchronizer) syncDatabases() error {
	dbnames, err := p.srcSession.DatabaseNames()
	if err != nil {
		return err
	}
	for _, dbname := range dbnames {
		if dbname != "local" && dbname != "admin" {
			if err := p.syncDatabase(dbname); err != nil {
				log.Println("sync database:", err)
			}
		}
	}
	return nil
}

func (p *Synchronizer) syncDatabase(dbname string) error {
	collnames, err := p.srcSession.DB(dbname).CollectionNames()
	if err != nil {
		return err
	}
	log.Printf("sync database '%s'\n", dbname)
	for _, collname := range collnames {
		// skip collections whose name starts with "system."
		if strings.Index(collname, "system.") == 0 {
			continue
		}
		log.Printf("\tsync collection '%s.%s'\n", dbname, collname)
		coll := p.srcSession.DB(dbname).C(collname)

		// create indexes
		if indexes, err := coll.Indexes(); err == nil {
			for _, index := range indexes {
				log.Println("\t\tcreate index:", index)
				if err := p.dstSession.DB(dbname).C(collname).EnsureIndex(index); err != nil {
					return err
				}
			}
		} else {
			return err
		}

		nworkers := runtime.NumCPU()
		docs := make(chan bson.M, 10000)
		done := make(chan struct{}, nworkers)
		for i := 0; i < nworkers; i++ {
			go p.write_document(i, dbname, collname, docs, done)
		}

		n := 0
		query := coll.Find(nil)
		cursor := query.Snapshot().Iter()
		total, _ := query.Count()

		for {
			var doc bson.M
			if cursor.Next(&doc) {
				docs <- doc
				n++
				if n%10000 == 0 {
					log.Printf("\t\t%s.%s %d/%d (%.2f%%)\n", dbname, collname, n, total, float64(n)/float64(total)*100)
				}
			} else {
				log.Printf("\t\t%s.%s %d/%d (%.2f%%)\n", dbname, collname, n, total, float64(n)/float64(total)*100)
				cursor.Close()
				close(docs)
				break
			}
		}

		// wait for all workers done
		for i := 0; i < nworkers; i++ {
			<-done
		}
	}
	return nil
}

func (p *Synchronizer) write_document(id int, dbname string, collname string, docs <-chan bson.M, done chan<- struct{}) {
	for doc := range docs {
		//if _, err := p.dstSession.Clone().DB(dbname).C(collname).UpsertId(doc["_id"], doc); err != nil {
		if err := p.dstSession.Clone().DB(dbname).C(collname).Insert(doc); err != nil {
			log.Println("write document:", err)
		}
	}
	done <- struct{}{}
}
