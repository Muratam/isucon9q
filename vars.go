package main

import (
	"html/template"
	"sync"
	"time"

	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
)

const (
	sessionName = "session_isucari"

	DefaultPaymentServiceURL  = "http://localhost:5555"
	DefaultShipmentServiceURL = "http://localhost:7000"

	ItemMinPrice    = 100
	ItemMaxPrice    = 1000000
	ItemPriceErrMsg = "商品価格は100ｲｽｺｲﾝ以上、1,000,000ｲｽｺｲﾝ以下にしてください"

	ItemStatusOnSale  = "on_sale"
	ItemStatusTrading = "trading"
	ItemStatusSoldOut = "sold_out"
	ItemStatusStop    = "stop"
	ItemStatusCancel  = "cancel"

	PaymentServiceIsucariAPIKey = "a15400e46c83635eb181-946abb51ff26a868317c"
	PaymentServiceIsucariShopID = "11"

	TransactionEvidenceStatusWaitShipping = "wait_shipping"
	TransactionEvidenceStatusWaitDone     = "wait_done"
	TransactionEvidenceStatusDone         = "done"

	ShippingsStatusInitial    = "initial"
	ShippingsStatusWaitPickup = "wait_pickup"
	ShippingsStatusShipping   = "shipping"
	ShippingsStatusDone       = "done"

	BumpChargeSeconds = 3 * time.Second

	ItemsPerPage        = 48
	TransactionsPerPage = 10

	BcryptCost          = 4
	MasterServerAddress = "172.24.122.186"
)

var (
	templates *template.Template
	dbx       *sqlx.DB
	store     sessions.Store
)

// とりあえず plain password だけを管理するサーバー(ID/AccountName/PlainPassword以外の情報は嘘)
var isMasterServerIP = IsMasterServerIP()

// string -> string
var accountNameToIDServer = NewMasterOrSlaveSyncMapServer(GetMasterServerAddress()+":8885", isMasterServerIP, DefaultSendCustomFunction)

// userId(string) -> User{}
var smUserServer = NewMasterOrSlaveSyncMapServer(GetMasterServerAddress()+":8884", isMasterServerIP, DefaultSendCustomFunction)

// itemid (string) -> isSold(bool) ()
var smItemPostBuyIsLockedServer = NewMasterOrSlaveSyncMapServer(GetMasterServerAddress()+":8886", isMasterServerIP, DefaultSendCustomFunction)

var postBuyLock = sync.Mutex{}
