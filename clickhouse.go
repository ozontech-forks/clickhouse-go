package clickhouse

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ClientName = "Golang SQLDriver"
)

const (
	ClickHouseRevision         = 54058
	ClickHouseDBMSVersionMajor = 1
	ClickHouseDBMSVersionMinor = 1
)

const (
	DefaultDatabase = "default"
	DefaultUsername = "default"
)

type logger func(format string, v ...interface{})

var (
	nolog    = func(string, ...interface{}) {}
	debuglog = log.New(os.Stdout, "[clickhouse]", 0).Printf
)

func init() {
	sql.Register("clickhouse", &clickhouse{})
}

type clickhouse struct {
	log                logger
	conn               *connect
	compress           bool
	serverName         string
	serverRevision     uint
	serverVersionMinor uint
	serverVersionMajor uint
	serverTimezone     *time.Location
}

func (ch *clickhouse) Open(dsn string) (driver.Conn, error) {
	url, err := url.Parse(dsn)
	if err != nil {
		return nil, err
	}
	var (
		hosts    = []string{url.Host}
		database = url.Query().Get("database")
		username = url.Query().Get("username")
		password = url.Query().Get("password")
	)
	if len(database) == 0 {
		database = DefaultDatabase
	}
	if len(username) == 0 {
		username = DefaultUsername
	}
	ch.log = nolog
	ch.serverTimezone = time.UTC
	if debug, err := strconv.ParseBool(url.Query().Get("debug")); err == nil && debug {
		ch.log = debuglog
	}
	if compress, err := strconv.ParseBool(url.Query().Get("compress")); err == nil {
		ch.compress = compress
	}
	if altHosts := strings.Split(url.Query().Get("alt_hosts"), ","); len(altHosts) != 0 {
		hosts = append(hosts, altHosts...)
	}
	if ch.conn, err = dial(url.Scheme, hosts); err != nil {
		return nil, err
	}
	if err := ch.hello(database, username, password); err != nil {
		return nil, err
	}
	return nil, nil
}

func (ch *clickhouse) Prepare(query string) (driver.Stmt, error) {
	return nil, nil
}

func (ch *clickhouse) Begin() (driver.Tx, error) {
	return nil, nil
}

func (ch *clickhouse) Close() error {
	return ch.conn.Close()
}

func (ch *clickhouse) hello(database, username, password string) error {
	ch.log("[hello] -> %s %d.%d.%d",
		ClientName,
		ClickHouseDBMSVersionMajor,
		ClickHouseDBMSVersionMinor,
		ClickHouseRevision,
	)
	{
		ch.conn.writeUInt(ClientHelloPacket)
		ch.conn.writeString(ClientName)
		ch.conn.writeUInt(ClickHouseDBMSVersionMajor)
		ch.conn.writeUInt(ClickHouseDBMSVersionMinor)
		ch.conn.writeUInt(ClickHouseRevision)
		ch.conn.writeString(database)
		ch.conn.writeString(username)
		ch.conn.writeString(password)
	}
	{
		packet, err := ch.conn.readUInt()
		if err != nil {
			return err
		}
		switch packet {
		case ServerExceptionPacket:
			return ch.exception()
		case ServerHelloPacket:
			var err error
			if ch.serverName, err = ch.conn.readString(); err != nil {
				return err
			}
			if ch.serverVersionMinor, err = ch.conn.readUInt(); err != nil {
				return err
			}
			if ch.serverVersionMajor, err = ch.conn.readUInt(); err != nil {
				return err
			}
			if ch.serverRevision, err = ch.conn.readUInt(); err != nil {
				return err
			}
			if ch.serverRevision >= DBMS_MIN_REVISION_WITH_SERVER_TIMEZONE {
				timezone, err := ch.conn.readString()
				if err != nil {
					return err
				}
				if ch.serverTimezone, err = time.LoadLocation(timezone); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("Unexpected packet from server")
		}
	}
	ch.log("[hello] <- %s %d.%d.%d (%s)",
		ch.serverName,
		ch.serverVersionMajor,
		ch.serverVersionMinor,
		ch.serverRevision,
		ch.serverTimezone,
	)
	return nil
}

type Exception struct {
	Code       int32
	Name       string
	Message    string
	StackTrace string
	nested     error
}

func (e *Exception) Error() string {
	return fmt.Sprintf("code: %d, message: %s", e.Code, e.Message)
}

func (ch *clickhouse) exception() error {
	var (
		e         Exception
		err       error
		hasNested bool
	)
	if e.Code, err = ch.conn.readBinaryInt32(); err != nil {
		return err
	}
	if e.Name, err = ch.conn.readString(); err != nil {
		return err
	}
	if e.Message, err = ch.conn.readString(); err != nil {
		return err
	}
	e.Message = strings.TrimSpace(strings.TrimPrefix(e.Message, e.Name+":"))
	if e.StackTrace, err = ch.conn.readString(); err != nil {
		return err
	}
	if hasNested, err = ch.conn.readBinaryBool(); err != nil {
		return err
	}
	if hasNested {
		e.nested = ch.exception()
	}
	return &e
}

func (ch *clickhouse) ping() error {
	ch.log("-> ping")
	if err := ch.conn.writeUInt(ClientPingPacket); err != nil {
		return err
	}
	packet, err := ch.conn.readUInt()
	if err != nil {
		return err
	}
	for packet == ServerProgressPacket {
		if _, err = ch.progress(); err != nil {
			return err
		}
		if packet, err = ch.conn.readUInt(); err != nil {
			return err
		}
	}
	if packet != ServerPongPacket {
		return fmt.Errorf("Unexpected packet from server")
	}
	ch.log("<- pong")
	return nil
}

type progress struct {
	rows      uint
	bytes     uint
	totalRows uint
}

func (ch *clickhouse) progress() (*progress, error) {
	var (
		p   progress
		err error
	)
	if p.rows, err = ch.conn.readUInt(); err != nil {
		return nil, err
	}
	if p.bytes, err = ch.conn.readUInt(); err != nil {
		return nil, err
	}
	if ch.serverRevision >= DBMS_MIN_REVISION_WITH_TOTAL_ROWS_IN_PROGRESS {
		if p.totalRows, err = ch.conn.readUInt(); err != nil {
			return nil, err
		}
	}
	return &p, nil
}
