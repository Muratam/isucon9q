package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func initializeDBtoOnMemory() {
	idToUserServer.FlushAll()
	accountNameToIDServer.FlushAll()
	idToItemServer.FlushAll()
	// init users
	users := make([]User, 0)
	err := dbx.Select(&users, "SELECT * FROM `users`")
	if err != nil {
		panic(err)
	}
	idToUserServerMap := map[string]interface{}{}
	accountNameToIDServerMap := map[string]interface{}{}
	for _, u := range users {
		u.PlainPassword = userIdToPlainPassword[int(u.ID)]
		key := strconv.Itoa(int(u.ID))
		idToUserServerMap[key] = u
		accountNameToIDServerMap[u.AccountName] = key
	}
	idToUserServer.MSet(idToUserServerMap)
	accountNameToIDServer.MSet(accountNameToIDServerMap)
	// init items
	items := make([]Item, 0)
	err = dbx.Select(&items, "SELECT * FROM `items`")
	if err != nil {
		panic(err)
	}
	idToItemServerMap := map[string]interface{}{}
	for _, item := range items {
		key := strconv.Itoa(int(item.ID))
		idToItemServerMap[key] = item
	}
	idToItemServer.MSet(idToItemServerMap)
}

func postInitialize(w http.ResponseWriter, r *http.Request) {
	ri := reqInitialize{}

	err := json.NewDecoder(r.Body).Decode(&ri)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	cmd := exec.Command("../sql/init.sh")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	cmd.Run()
	if err != nil {
		outputErrorMsg(w, http.StatusInternalServerError, "exec init.sh error")
		return
	}

	_, err = dbx.Exec(
		"INSERT INTO `configs` (`name`, `val`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `val` = VALUES(`val`)",
		"payment_service_url",
		ri.PaymentServiceURL,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		return
	}
	_, err = dbx.Exec(
		"INSERT INTO `configs` (`name`, `val`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `val` = VALUES(`val`)",
		"shipment_service_url",
		ri.ShipmentServiceURL,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		return
	}
	initializeDBtoOnMemory()

	res := resInitialize{
		// キャンペーン実施時には還元率の設定を返す。詳しくはマニュアルを参照のこと。
		Campaign: 4,
		// 実装言語を返す
		Language: "Go",
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(res)
}

func postItemEdit(w http.ResponseWriter, r *http.Request) {
	rie := reqItemEdit{}
	err := json.NewDecoder(r.Body).Decode(&rie)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := rie.CSRFToken
	itemID := rie.ItemID
	price := rie.ItemPrice

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	if price < ItemMinPrice || price > ItemMaxPrice {
		outputErrorMsg(w, http.StatusBadRequest, ItemPriceErrMsg)
		return
	}

	seller, errCode, errMsg := getUser(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}
	now := time.Now().Truncate(time.Second)
	targetItem := Item{}
	itemIDStr := strconv.Itoa(int(itemID))
	idToItemServer.Transaction(itemIDStr, func(tx KeyValueStoreConn) {
		ok := tx.Get(itemIDStr, &targetItem)
		if !ok {
			outputErrorMsg(w, http.StatusNotFound, "item not found")
			return
		}
		if targetItem.SellerID != seller.ID {
			outputErrorMsg(w, http.StatusForbidden, "自分の商品以外は編集できません")
			return
		}
		if targetItem.Status != ItemStatusOnSale {
			outputErrorMsg(w, http.StatusForbidden, "販売中の商品以外編集できません")
			return
		}
		targetItem.Price = price
		targetItem.UpdatedAt = now
		tx.Set(itemIDStr, targetItem)
		dbx.Exec("UPDATE `items` SET `price` = ?, `updated_at` = ? WHERE `id` = ?", price, now, itemID)
	})
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(&resItemEdit{
		ItemID:        targetItem.ID,
		ItemPrice:     targetItem.Price,
		ItemCreatedAt: targetItem.CreatedAt.Unix(),
		ItemUpdatedAt: targetItem.UpdatedAt.Unix(),
	})
}

var isBoughtByKey sync.Map // rb.ItemID -> (chan int)

func postBuy(w http.ResponseWriter, r *http.Request) {
	rb := reqBuy{}

	err := json.NewDecoder(r.Body).Decode(&rb)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	if rb.CSRFToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	buyer, errCode, errMsg := getUser(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}
	chanBoughtExistanceI := make(chan bool, 1) // 一人目だけが購入できる NOTE: make(chan bool) だとデッドロック
	chanBoughtExistance := &chanBoughtExistanceI
	chanBoughtPre, channExists := isBoughtByKey.LoadOrStore(rb.ItemID, chanBoughtExistance)
	if channExists {
		chanBoughtExistance = chanBoughtPre.(*(chan bool))
		isBought := <-*chanBoughtExistance
		if isBought {
			outputErrorMsg(w, http.StatusForbidden, "item is not for sale")
			*chanBoughtExistance <- true
			return
		}
	}

	targetItem := Item{}
	itemIdStr := strconv.Itoa(int(rb.ItemID))
	successed := false
	idToItemServer.Transaction(itemIdStr, func(tx KeyValueStoreConn) {
		ok := tx.Get(itemIdStr, &targetItem)
		if !ok {
			outputErrorMsg(w, http.StatusNotFound, "item not found")
			return
		}
		log.Println(itemIdStr, " will be bought")
		if targetItem.Status != ItemStatusOnSale {
			outputErrorMsg(w, http.StatusForbidden, "item is not for sale")
			return
		}
		if targetItem.SellerID == buyer.ID {
			outputErrorMsg(w, http.StatusForbidden, "自分の商品は買えません")
			return
		}
		seller := User{}
		sellerIDStr := strconv.Itoa(int(targetItem.SellerID))
		exists := idToUserServer.Get(sellerIDStr, &seller)
		if !exists {
			outputErrorMsg(w, http.StatusNotFound, "seller not found")
			return
		}
		category, err := getCategoryByID(dbx, targetItem.CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusInternalServerError, "category id error")
			return
		}
		type ScrErr struct {
			scr *APIShipmentCreateRes
			err error
		}
		type PstrErr struct {
			pstr *APIPaymentServiceTokenRes
			err  error
		}
		chScrErr := make(chan ScrErr, 1)
		go func() {
			scr, err := APIShipmentCreate(getShipmentServiceURL(), &APIShipmentCreateReq{
				ToAddress:   buyer.Address,
				ToName:      buyer.AccountName,
				FromAddress: seller.Address,
				FromName:    seller.AccountName,
			})
			chScrErr <- ScrErr{scr, err}
		}()
		chPstrErr := make(chan PstrErr, 1)
		go func() {
			pstr, err := APIPaymentToken(getPaymentServiceURL(), &APIPaymentServiceTokenReq{
				ShopID: PaymentServiceIsucariShopID,
				Token:  rb.Token,
				APIKey: PaymentServiceIsucariAPIKey,
				Price:  targetItem.Price,
			})
			chPstrErr <- PstrErr{pstr, err}
		}()
		scrErr := <-chScrErr
		scr, err := scrErr.scr, scrErr.err
		if err != nil {
			outputErrorMsg(w, http.StatusInternalServerError, "failed to request to shipment service")
			return
		}
		pstrErr := <-chPstrErr
		pstr, err := pstrErr.pstr, pstrErr.err
		if err != nil {
			outputErrorMsg(w, http.StatusInternalServerError, "payment service is failed")
			return
		}
		if pstr.Status == "invalid" {
			outputErrorMsg(w, http.StatusBadRequest, "カード情報に誤りがあります")
			return
		}
		if pstr.Status == "fail" {
			outputErrorMsg(w, http.StatusBadRequest, "カードの残高が足りません")
			return
		}
		if pstr.Status != "ok" {
			outputErrorMsg(w, http.StatusBadRequest, "想定外のエラー")
			return
		}
		log.Println(itemIdStr, " will try to be bought")
		// NOTE: 成功すると楽観する
		result, err := dbx.Exec("INSERT INTO `transaction_evidences` (`seller_id`, `buyer_id`, `status`, `item_id`, `item_name`, `item_price`, `item_description`,`item_category_id`,`item_root_category_id`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			targetItem.SellerID,
			buyer.ID,
			TransactionEvidenceStatusWaitShipping,
			targetItem.ID,
			targetItem.Name,
			targetItem.Price,
			targetItem.Description,
			category.ID,
			category.ParentID,
		)
		if err != nil {
			log.Println(err, "At INSERTING TRANSACTION EVIDENCES")
		} else {
			log.Println(targetItem, ":", targetItem.ID, "BOUGHT")
		}
		now := time.Now().Truncate(time.Second)
		transactionEvidenceID, _ := result.LastInsertId()
		targetItem.BuyerID = buyer.ID
		targetItem.Status = ItemStatusTrading
		targetItem.UpdatedAt = now
		// ロールバックされうるので遅延
		tx.Set(itemIdStr, targetItem)
		dbx.Exec("UPDATE `items` SET `buyer_id` = ?, `status` = ?, `updated_at` = ? WHERE `id` = ?",
			buyer.ID,
			ItemStatusTrading,
			now,
			targetItem.ID,
		)
		dbx.Exec("INSERT INTO `shippings` (`transaction_evidence_id`, `status`, `item_name`, `item_id`, `reserve_id`, `reserve_time`, `to_address`, `to_name`, `from_address`, `from_name`, `img_binary`) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
			transactionEvidenceID,
			ShippingsStatusInitial,
			targetItem.Name,
			targetItem.ID,
			scr.ReserveID,
			scr.ReserveTime,
			buyer.Address,
			buyer.AccountName,
			seller.Address,
			seller.AccountName,
			"",
		)
		w.Header().Set("Content-Type", "application/json;charset=utf-8")
		json.NewEncoder(w).Encode(resBuy{TransactionEvidenceID: transactionEvidenceID})
		successed = true
	})
	*chanBoughtExistance <- successed
}

func postShip(w http.ResponseWriter, r *http.Request) {
	reqps := reqPostShip{}

	err := json.NewDecoder(r.Body).Decode(&reqps)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := reqps.CSRFToken
	itemID := reqps.ItemID

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	seller, errCode, errMsg := getUser(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	transactionEvidence := TransactionEvidence{}

	tx := dbx.MustBegin()

	err = tx.Get(&transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		tx.Rollback()
		return
	}

	if transactionEvidence.SellerID != seller.ID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		tx.Rollback()
		return
	}

	if transactionEvidence.Status != TransactionEvidenceStatusWaitShipping {
		outputErrorMsg(w, http.StatusForbidden, "準備ができていません")
		tx.Rollback()
		return
	}

	shipping := Shipping{}
	err = tx.Get(&shipping, "SELECT * FROM `shippings` WHERE `transaction_evidence_id` = ? FOR UPDATE", transactionEvidence.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "shippings not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		tx.Rollback()
		return
	}

	img, err := APIShipmentRequest(getShipmentServiceURL(), &APIShipmentRequestReq{
		ReserveID: shipping.ReserveID,
	})
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "failed to request to shipment service")
		tx.Rollback()

		return
	}

	_, err = tx.Exec("UPDATE `shippings` SET `status` = ?, `img_binary` = ?, `updated_at` = ? WHERE `transaction_evidence_id` = ?",
		ShippingsStatusWaitPickup,
		img,
		time.Now(),
		transactionEvidence.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		tx.Rollback()
		return
	}

	tx.Commit()

	rps := resPostShip{
		Path:      fmt.Sprintf("/transactions/%d.png", transactionEvidence.ID),
		ReserveID: shipping.ReserveID,
	}
	json.NewEncoder(w).Encode(rps)
}

func postShipDone(w http.ResponseWriter, r *http.Request) {
	reqpsd := reqPostShipDone{}

	err := json.NewDecoder(r.Body).Decode(&reqpsd)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := reqpsd.CSRFToken
	itemID := reqpsd.ItemID

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	seller, errCode, errMsg := getUser(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	tx := dbx.MustBegin()

	transactionEvidence := TransactionEvidence{}
	err = tx.Get(&transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidence not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		tx.Rollback()
		return
	}

	if transactionEvidence.SellerID != seller.ID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		tx.Rollback()
		return
	}

	if transactionEvidence.Status != TransactionEvidenceStatusWaitShipping {
		outputErrorMsg(w, http.StatusForbidden, "準備ができていません")
		tx.Rollback()
		return
	}

	shipping := Shipping{}
	err = tx.Get(&shipping, "SELECT * FROM `shippings` WHERE `transaction_evidence_id` = ? FOR UPDATE", transactionEvidence.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "shippings not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		tx.Rollback()
		return
	}

	ssr, err := APIShipmentStatus(getShipmentServiceURL(), &APIShipmentStatusReq{
		ReserveID: shipping.ReserveID,
	})
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "failed to request to shipment service")
		tx.Rollback()

		return
	}

	if !(ssr.Status == ShippingsStatusShipping || ssr.Status == ShippingsStatusDone) {
		outputErrorMsg(w, http.StatusForbidden, "shipment service側で配送中か配送完了になっていません")
		tx.Rollback()
		return
	}

	_, err = tx.Exec("UPDATE `shippings` SET `status` = ?, `updated_at` = ? WHERE `transaction_evidence_id` = ?",
		ssr.Status,
		time.Now(),
		transactionEvidence.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		tx.Rollback()
		return
	}

	_, err = tx.Exec("UPDATE `transaction_evidences` SET `status` = ?, `updated_at` = ? WHERE `id` = ?",
		TransactionEvidenceStatusWaitDone,
		time.Now(),
		transactionEvidence.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		tx.Rollback()
		return
	}

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resBuy{TransactionEvidenceID: transactionEvidence.ID})
}

func postComplete(w http.ResponseWriter, r *http.Request) {
	reqpc := reqPostComplete{}
	err := json.NewDecoder(r.Body).Decode(&reqpc)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}
	csrfToken := reqpc.CSRFToken
	itemID := reqpc.ItemID
	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}
	buyer, errCode, errMsg := getUser(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}
	itemIdStr := strconv.Itoa(int(itemID))
	successed := false
	idToItemServer.Transaction(itemIdStr, func(tx KeyValueStoreConn) {
		item := Item{}
		ok := tx.Get(itemIdStr, &item)
		if !ok {
			outputErrorMsg(w, http.StatusNotFound, "items not found")
			return
		}
		if item.Status != ItemStatusTrading {
			outputErrorMsg(w, http.StatusForbidden, "商品が取引中ではありません")
			return
		}
		transactionEvidence := TransactionEvidence{}
		err = dbx.Get(&transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ?", itemID)
		if err == sql.ErrNoRows {
			outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
			return
		}
		if transactionEvidence.BuyerID != buyer.ID {
			outputErrorMsg(w, http.StatusForbidden, "権限がありません")
			return
		}
		if transactionEvidence.Status != TransactionEvidenceStatusWaitDone {
			outputErrorMsg(w, http.StatusForbidden, "準備ができていません")
			return
		}
		shipping := Shipping{}
		err = dbx.Get(&shipping, "SELECT * FROM `shippings` WHERE `transaction_evidence_id` = ?", transactionEvidence.ID)
		if err != nil {
			outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
			return
		}
		ssr, err := APIShipmentStatus(getShipmentServiceURL(), &APIShipmentStatusReq{
			ReserveID: shipping.ReserveID,
		})
		if err != nil {
			outputErrorMsg(w, http.StatusInternalServerError, "failed to request to shipment service")
			return
		}
		if !(ssr.Status == ShippingsStatusDone) {
			outputErrorMsg(w, http.StatusBadRequest, "shipment service側で配送完了になっていません")
			return
		}
		// 楽観
		now := time.Now().Truncate(time.Second)
		dbx.Exec("UPDATE `shippings` SET `status` = ?, `updated_at` = ? WHERE `transaction_evidence_id` = ?",
			ShippingsStatusDone,
			now,
			transactionEvidence.ID,
		)
		dbx.Exec("UPDATE `transaction_evidences` SET `status` = ?, `updated_at` = ? WHERE `id` = ?",
			TransactionEvidenceStatusDone,
			now,
			transactionEvidence.ID,
		)
		item.UpdatedAt = now
		item.Status = ItemStatusSoldOut
		tx.Set(itemIdStr, item)
		dbx.Exec("UPDATE `items` SET `status` = ?, `updated_at` = ? WHERE `id` = ?",
			ItemStatusSoldOut,
			now,
			itemID,
		)
		w.Header().Set("Content-Type", "application/json;charset=utf-8")
		json.NewEncoder(w).Encode(resBuy{TransactionEvidenceID: transactionEvidence.ID})
		successed = true
	})
}

func postSell(w http.ResponseWriter, r *http.Request) {
	csrfToken := r.FormValue("csrf_token")
	name := r.FormValue("name")
	description := r.FormValue("description")
	priceStr := r.FormValue("price")
	categoryIDStr := r.FormValue("category_id")

	f, header, err := r.FormFile("image")
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusBadRequest, "image error")
		return
	}
	defer f.Close()

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")
		return
	}

	categoryID, err := strconv.Atoi(categoryIDStr)
	if err != nil || categoryID < 0 {
		outputErrorMsg(w, http.StatusBadRequest, "category id error")
		return
	}

	price, err := strconv.Atoi(priceStr)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "price error")
		return
	}

	if name == "" || description == "" || price == 0 || categoryID == 0 {
		outputErrorMsg(w, http.StatusBadRequest, "all parameters are required")

		return
	}

	if price < ItemMinPrice || price > ItemMaxPrice {
		outputErrorMsg(w, http.StatusBadRequest, ItemPriceErrMsg)

		return
	}

	category, err := getCategoryByID(dbx, categoryID)
	if err != nil || category.ParentID == 0 {
		log.Print(categoryID, category)
		outputErrorMsg(w, http.StatusBadRequest, "Incorrect category ID")
		return
	}

	user, errCode, errMsg := getUser(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	img, err := ioutil.ReadAll(f)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "image error")
		return
	}

	ext := filepath.Ext(header.Filename)

	if !(ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif") {
		outputErrorMsg(w, http.StatusBadRequest, "unsupported image format error")
		return
	}

	if ext == ".jpeg" {
		ext = ".jpg"
	}

	imgName := fmt.Sprintf("%s%s", secureRandomStr(16), ext)
	err = ioutil.WriteFile(fmt.Sprintf("../public/upload/%s", imgName), img, 0644)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "Saving image failed")
		return
	}
	strUserId := strconv.Itoa(int(user.ID))
	if !idToUserServer.Exists(strUserId) {
		outputErrorMsg(w, http.StatusNotFound, "user not found")
		return
	}
	idToUserServer.Transaction(strUserId, func(utx KeyValueStoreConn) {
		seller := User{}
		utx.Get(strUserId, &seller)
		item := Item{
			SellerID:    seller.ID,
			Status:      ItemStatusOnSale,
			Name:        name,
			Price:       price,
			Description: description,
			ImageName:   imgName,
			CategoryID:  category.ID,
		}
		for {
			itemID := idToItemServer.DBSize() + 1
			itemIDStr := strconv.Itoa(itemID)
			now := time.Now().Truncate(time.Second)
			item.CreatedAt = now
			item.UpdatedAt = now
			item.TimeDateID = now.Format("20060102150405") + fmt.Sprintf("%08d", itemID)
			successed := false
			idToItemServer.Transaction(itemIDStr, func(tx KeyValueStoreConn) {
				if tx.Exists(itemIDStr) {
					return
				}
				tx.Set(itemIDStr, item)
				dbx.Exec("INSERT INTO `items` (`seller_id`, `status`, `name`, `price`, `description`,`image_name`,`category_id`, `created_at`, `updated_at`, `timedateid`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
					item.SellerID,
					item.Status,
					item.Name,
					item.Price,
					item.Description,
					item.ImageName,
					item.CategoryID,
					item.CreatedAt,
					item.UpdatedAt,
					item.TimeDateID,
				)
				successed = true
			})
			if !successed {
				continue
			}
			seller.NumSellItems += 1
			seller.LastBump = now
			utx.Set(strUserId, seller)
			w.Header().Set("Content-Type", "application/json;charset=utf-8")
			json.NewEncoder(w).Encode(resSell{ID: int64(itemID)})
			break
		}
	})
}

func postBump(w http.ResponseWriter, r *http.Request) {
	rb := reqBump{}
	err := json.NewDecoder(r.Body).Decode(&rb)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}
	csrfToken := rb.CSRFToken
	itemID := rb.ItemID
	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")
		return
	}
	user, errCode, errMsg := getUser(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}
	uidStr := strconv.Itoa(int(user.ID))
	if !idToUserServer.Exists(uidStr) {
		outputErrorMsg(w, http.StatusNotFound, "user not found")
		return
	}

	idToUserServer.Transaction(uidStr, func(utx KeyValueStoreConn) {
		itemIDStr := strconv.Itoa(int(itemID))
		idToItemServer.Transaction(itemIDStr, func(itx KeyValueStoreConn) {
			targetItem := Item{}
			ok := itx.Get(itemIDStr, &targetItem)
			if !ok {
				outputErrorMsg(w, http.StatusNotFound, "item not found")
				return
			}
			if targetItem.SellerID != user.ID {
				outputErrorMsg(w, http.StatusForbidden, "自分の商品以外は編集できません")
				return
			}
			seller := User{}
			utx.Get(uidStr, &seller)
			now := time.Now().Truncate(time.Second)
			// last_bump + 3s > now
			if seller.LastBump.Add(BumpChargeSeconds).After(now) {
				outputErrorMsg(w, http.StatusForbidden, "Bump not allowed")
				return
			}
			targetItem.CreatedAt = now
			targetItem.UpdatedAt = now
			targetItem.TimeDateID = now.Format("20060102150405") + fmt.Sprintf("%08d", targetItem.ID)
			itx.Set(itemIDStr, targetItem)
			dbx.Exec("UPDATE `items` SET `created_at`=?, `updated_at`=?, `timedateid`=? WHERE id=?",
				targetItem.CreatedAt,
				targetItem.UpdatedAt,
				targetItem.TimeDateID,
				targetItem.ID,
			)
			seller.LastBump = now
			utx.Set(uidStr, seller)
			w.Header().Set("Content-Type", "application/json;charset=utf-8")
			json.NewEncoder(w).Encode(&resItemEdit{
				ItemID:        targetItem.ID,
				ItemPrice:     targetItem.Price,
				ItemCreatedAt: targetItem.CreatedAt.Unix(),
				ItemUpdatedAt: targetItem.UpdatedAt.Unix(),
			})
		})
	})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	rl := reqLogin{}
	err := json.NewDecoder(r.Body).Decode(&rl)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	accountName := rl.AccountName
	password := rl.Password

	if accountName == "" || password == "" {
		outputErrorMsg(w, http.StatusBadRequest, "all parameters are required")
		return
	}
	if !accountNameToIDServer.Exists(accountName) {
		outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
		return
	}
	idStr := ""
	accountNameToIDServer.Get(accountName, &idStr)
	u := User{}
	idToUserServer.Get(idStr, &u)
	if strings.Compare(u.PlainPassword, password) != 0 {
		outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
		return
	}
	session := getSession(r)
	session.Values["user_id"] = u.ID
	session.Values["csrf_token"] = secureRandomStr(20)
	if err = session.Save(r, w); err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "session error")
		return
	}
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(u)
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	rr := reqRegister{}
	err := json.NewDecoder(r.Body).Decode(&rr)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	accountName := rr.AccountName
	address := rr.Address
	password := rr.Password

	if accountName == "" || password == "" || address == "" {
		outputErrorMsg(w, http.StatusBadRequest, "all parameters are required")

		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "error")
		return
	}
	// WARN: 合ってる？
	// トランザクションが必要？(ID)
	var newUser User
	newUser.ID = int64(idToUserServer.DBSize() + 1)
	newUser.AccountName = accountName
	newUser.HashedPassword = hashedPassword
	newUser.Address = address
	// WARN
	newUser.NumSellItems = 0
	t, _ := time.Parse("2006-01-02 15:04:05", "2000-01-01 00:00:00")
	newUser.LastBump = t
	newUser.CreatedAt = time.Now() // CURRENT_TIMESTAMP
	newUser.PlainPassword = password
	idStr := strconv.Itoa(int(newUser.ID))
	idToUserServer.Set(idStr, newUser)
	accountNameToIDServer.Set(newUser.AccountName, idStr)
	session := getSession(r)
	session.Values["user_id"] = newUser.ID
	session.Values["csrf_token"] = secureRandomStr(20)
	if err = session.Save(r, w); err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "session error")
		return
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(newUser)
}
