package canal

import (
	"regexp"
	"time"

	"context"
	//"golang.org/x/net/context"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/siddontang/go-mysql/mysql"
	"github.com/siddontang/go-mysql/replication"
)

var (
	expAlterTable = regexp.MustCompile("(?i)^ALTER\\sTABLE\\s.*?`{0,1}(.*?)`{0,1}\\.{0,1}`{0,1}([^`\\.]+?)`{0,1}\\s.*")
)

func (c *Canal) startSyncBinlog() error {
	pos := mysql.Position{c.master.Name, c.master.Position}

	log.Infof("Start sync binlog at %v", pos)

	s, err := c.syncer.StartSync(pos)
	if err != nil {
		return errors.Errorf("start sync replication at %v error %v", pos, err)
	}

	timeout := time.Second
	forceSavePos := false
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ev, err := s.GetEvent(ctx)
		cancel()

		if err == context.DeadlineExceeded {
			timeout = 2 * timeout
			continue
		}

		if err != nil {
			return errors.Trace(err)
		}

		timeout = time.Second

		//next binlog pos
		pos.Pos = ev.Header.LogPos

		forceSavePos = false

		// We only save position with RotateEvent and XIDEvent.
		// For RowsEvent, we can't save the position until meeting XIDEvent
		// which tells the whole transaction is over.
		// TODO: If we meet any DDL query, we must save too.
		switch e := ev.Event.(type) {
		case *replication.RotateEvent:
			pos.Name = string(e.NextLogName)
			pos.Pos = uint32(e.Position)
			// r.ev <- pos
			forceSavePos = true
			log.Debugf("rotate binlog to %v", pos)
		case *replication.RowsEvent:
			// we only focus row based event
			if err = c.handleRowsEvent(ev); err != nil {
				log.Errorf("handle rows event error %v", err)
				return errors.Trace(err)
			}
			// continue
		case *replication.XIDEvent:
			// try to save the position later
		case *replication.QueryEvent:
			// 暂时屏蔽该操作
			// continue
			// handle alert table query
			if mb := expAlterTable.FindSubmatch(e.Query); mb != nil {
				if len(mb[1]) == 0 {
					mb[1] = e.Schema
				}
				c.ClearTableCache(mb[1], mb[2])
				// alter will be ignore for filtercols func running

				if FilterTabs[string(mb[2])] {
					log.Warningf("table structure changed, but will be ignore for filtercols or optimus func of %s.%s\n", mb[1], mb[2])
					c.master.Update(pos.Name, pos.Pos, "", 0, -1, "")
					c.master.Save(forceSavePos)
					continue
				}

				if _, ok := Cfg_Tc[string(mb[2])]; ok {
					log.Warningf("table structure changed, but will be ignore for filtercols or optimus func of %s.%s\n", mb[1], mb[2])
					c.master.Update(pos.Name, pos.Pos, "", 0, -1, "")
					c.master.Save(forceSavePos)
					continue
				}

				if err = c.handleQueryEvent(ev, string(mb[1]), string(mb[2])); err != nil {
					log.Errorf("handle Query event(%s:%d) error %v", pos.Name, pos.Pos, err)
					return errors.Trace(err)
				}

				log.Infof("table structure changed, clear table cache: %s.%s\n", mb[1], mb[2])
				forceSavePos = true
			} else {
				// skip others
				continue
			}
		default:
			continue
		}

		c.master.Update(pos.Name, pos.Pos, "", 0, -1, "")
		c.master.Save(forceSavePos)
	}

	return nil
}

func (c *Canal) handleQueryEvent(e *replication.BinlogEvent, schema string, table string) error {

	// 屏蔽掉Query操作，暂时不忘kafka中推送类似数据
	// return nil

	ev := e.Event.(*replication.QueryEvent)

	t, err := c.GetTable(schema, table)
	if err != nil {
		return errors.Trace(err)
	}

	var action string
	action = AlterAction
	events := newQueryEvent(t, action, ev.Query)
	return c.travelQueryEventHandler(events)
}

func (c *Canal) handleRowsEvent(e *replication.BinlogEvent) error {
	ev := e.Event.(*replication.RowsEvent)

	// Caveat: table may be altered at runtime.
	schema := string(ev.Table.Schema)
	table := string(ev.Table.Table)

	t, err := c.GetTable(schema, table)
	if err != nil {
		return errors.Trace(err)
	}
	var action string
	switch e.Header.EventType {
	case replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2:
		action = InsertAction
	case replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
		action = DeleteAction
	case replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2:
		action = UpdateAction
	default:
		return errors.Errorf("%s not supported now", e.Header.EventType)
	}
	events := newRowsEvent(t, action, ev.Rows)
	return c.travelRowsEventHandler(events)
}

func (c *Canal) WaitUntilPos(pos mysql.Position, timeout int) error {
	if timeout <= 0 {
		timeout = 60
	}

	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	for {
		select {
		case <-timer.C:
			return errors.Errorf("wait position %v err", pos)
		default:
			curpos := c.master.Pos()
			if curpos.Compare(pos) >= 0 {
				return nil
			} else {
				time.Sleep(100 * time.Millisecond)
			}
		}
	}

	return nil
}

func (c *Canal) CatchMasterPos(timeout int) error {
	rr, err := c.Execute("SHOW MASTER STATUS")
	if err != nil {
		return errors.Trace(err)
	}

	name, _ := rr.GetString(0, 0)
	pos, _ := rr.GetInt(0, 1)

	return c.WaitUntilPos(mysql.Position{name, uint32(pos)}, timeout)
}

func (c *Canal) GetMasterPos() (string, uint32, error) {
	rr, err := c.Execute("SHOW MASTER STATUS")
	if err != nil {
		return "", 0, errors.Trace(err)
	}

	name, _ := rr.GetString(0, 0)
	pos, _ := rr.GetInt(0, 1)

	return name, uint32(pos), nil
}