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
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}
	_, err = dbx.Exec(
		"INSERT INTO `configs` (`name`, `val`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `val` = VALUES(`val`)",
		"shipment_service_url",
		ri.ShipmentServiceURL,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	res := resInitialize{
		// キャンペーン実施時には還元率の設定を返す。詳しくはマニュアルを参照のこと。
		Campaign: 2,
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

	targetItem := Item{}
	err = dbx.Get(&targetItem, "SELECT * FROM `items` WHERE `id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		return
	}
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	if targetItem.SellerID != seller.ID {
		outputErrorMsg(w, http.StatusForbidden, "自分の商品以外は編集できません")
		return
	}

	tx := dbx.MustBegin()
	err = tx.Get(&targetItem, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if targetItem.Status != ItemStatusOnSale {
		outputErrorMsg(w, http.StatusForbidden, "販売中の商品以外編集できません")
		tx.Rollback()
		return
	}

	_, err = tx.Exec("UPDATE `items` SET `price` = ?, `updated_at` = ? WHERE `id` = ?",
		price,
		time.Now(),
		itemID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	err = tx.Get(&targetItem, "SELECT * FROM `items` WHERE `id` = ?", itemID)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(&resItemEdit{
		ItemID:        targetItem.ID,
		ItemPrice:     targetItem.Price,
		ItemCreatedAt: targetItem.CreatedAt.Unix(),
		ItemUpdatedAt: targetItem.UpdatedAt.Unix(),
	})
}

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

	tx := dbx.MustBegin()

	targetItem := Item{}
	err = tx.Get(&targetItem, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", rb.ItemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if targetItem.Status != ItemStatusOnSale {
		outputErrorMsg(w, http.StatusForbidden, "item is not for sale")
		tx.Rollback()
		return
	}

	if targetItem.SellerID == buyer.ID {
		outputErrorMsg(w, http.StatusForbidden, "自分の商品は買えません")
		tx.Rollback()
		return
	}

	seller := User{}
	err = tx.Get(&seller, "SELECT * FROM `users` WHERE `id` = ? FOR UPDATE", targetItem.SellerID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "seller not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	category, err := getCategoryByID(tx, targetItem.CategoryID)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "category id error")
		tx.Rollback()
		return
	}

	result, err := tx.Exec("INSERT INTO `transaction_evidences` (`seller_id`, `buyer_id`, `status`, `item_id`, `item_name`, `item_price`, `item_description`,`item_category_id`,`item_root_category_id`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
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
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	transactionEvidenceID, err := result.LastInsertId()
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	_, err = tx.Exec("UPDATE `items` SET `buyer_id` = ?, `status` = ?, `updated_at` = ? WHERE `id` = ?",
		buyer.ID,
		ItemStatusTrading,
		time.Now(),
		targetItem.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	scr, err := APIShipmentCreate(getShipmentServiceURL(), &APIShipmentCreateReq{
		ToAddress:   buyer.Address,
		ToName:      buyer.AccountName,
		FromAddress: seller.Address,
		FromName:    seller.AccountName,
	})
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "failed to request to shipment service")
		tx.Rollback()

		return
	}

	pstr, err := APIPaymentToken(getPaymentServiceURL(), &APIPaymentServiceTokenReq{
		ShopID: PaymentServiceIsucariShopID,
		Token:  rb.Token,
		APIKey: PaymentServiceIsucariAPIKey,
		Price:  targetItem.Price,
	})
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "payment service is failed")
		tx.Rollback()
		return
	}

	if pstr.Status == "invalid" {
		outputErrorMsg(w, http.StatusBadRequest, "カード情報に誤りがあります")
		tx.Rollback()
		return
	}

	if pstr.Status == "fail" {
		outputErrorMsg(w, http.StatusBadRequest, "カードの残高が足りません")
		tx.Rollback()
		return
	}

	if pstr.Status != "ok" {
		outputErrorMsg(w, http.StatusBadRequest, "想定外のエラー")
		tx.Rollback()
		return
	}

	_, err = tx.Exec("INSERT INTO `shippings` (`transaction_evidence_id`, `status`, `item_name`, `item_id`, `reserve_id`, `reserve_time`, `to_address`, `to_name`, `from_address`, `from_name`, `img_binary`) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
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
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resBuy{TransactionEvidenceID: transactionEvidenceID})
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
	err = dbx.Get(&transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")

		return
	}

	if transactionEvidence.SellerID != seller.ID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		return
	}

	tx := dbx.MustBegin()

	item := Item{}
	err = tx.Get(&item, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if item.Status != ItemStatusTrading {
		outputErrorMsg(w, http.StatusForbidden, "商品が取引中ではありません")
		tx.Rollback()
		return
	}

	err = tx.Get(&transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `id` = ? FOR UPDATE", transactionEvidence.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
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
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
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

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
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

	transactionEvidence := TransactionEvidence{}
	err = dbx.Get(&transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidence not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")

		return
	}

	if transactionEvidence.SellerID != seller.ID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		return
	}

	tx := dbx.MustBegin()

	item := Item{}
	err = tx.Get(&item, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "items not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if item.Status != ItemStatusTrading {
		outputErrorMsg(w, http.StatusForbidden, "商品が取引中ではありません")
		tx.Rollback()
		return
	}

	err = tx.Get(&transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `id` = ? FOR UPDATE", transactionEvidence.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
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
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
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

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
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

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
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

	transactionEvidence := TransactionEvidence{}
	err = dbx.Get(&transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidence not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")

		return
	}

	if transactionEvidence.BuyerID != buyer.ID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		return
	}

	tx := dbx.MustBegin()
	item := Item{}
	err = tx.Get(&item, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "items not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if item.Status != ItemStatusTrading {
		outputErrorMsg(w, http.StatusForbidden, "商品が取引中ではありません")
		tx.Rollback()
		return
	}

	err = tx.Get(&transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `item_id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if transactionEvidence.Status != TransactionEvidenceStatusWaitDone {
		outputErrorMsg(w, http.StatusForbidden, "準備ができていません")
		tx.Rollback()
		return
	}

	shipping := Shipping{}
	err = tx.Get(&shipping, "SELECT * FROM `shippings` WHERE `transaction_evidence_id` = ? FOR UPDATE", transactionEvidence.ID)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
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

	if !(ssr.Status == ShippingsStatusDone) {
		outputErrorMsg(w, http.StatusBadRequest, "shipment service側で配送完了になっていません")
		tx.Rollback()
		return
	}

	_, err = tx.Exec("UPDATE `shippings` SET `status` = ?, `updated_at` = ? WHERE `transaction_evidence_id` = ?",
		ShippingsStatusDone,
		time.Now(),
		transactionEvidence.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	_, err = tx.Exec("UPDATE `transaction_evidences` SET `status` = ?, `updated_at` = ? WHERE `id` = ?",
		TransactionEvidenceStatusDone,
		time.Now(),
		transactionEvidence.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	_, err = tx.Exec("UPDATE `items` SET `status` = ?, `updated_at` = ? WHERE `id` = ?",
		ItemStatusSoldOut,
		time.Now(),
		itemID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resBuy{TransactionEvidenceID: transactionEvidence.ID})
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

	tx := dbx.MustBegin()

	seller := User{}
	err = tx.Get(&seller, "SELECT * FROM `users` WHERE `id` = ? FOR UPDATE", user.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "user not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	result, err := tx.Exec("INSERT INTO `items` (`seller_id`, `status`, `name`, `price`, `description`,`image_name`,`category_id`) VALUES (?, ?, ?, ?, ?, ?, ?)",
		seller.ID,
		ItemStatusOnSale,
		name,
		price,
		description,
		imgName,
		category.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	itemID, err := result.LastInsertId()
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	now := time.Now()
	_, err = tx.Exec("UPDATE `users` SET `num_sell_items`=?, `last_bump`=? WHERE `id`=?",
		seller.NumSellItems+1,
		now,
		seller.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}
	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resSell{ID: itemID})
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

	tx := dbx.MustBegin()

	targetItem := Item{}
	err = tx.Get(&targetItem, "SELECT * FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if targetItem.SellerID != user.ID {
		outputErrorMsg(w, http.StatusForbidden, "自分の商品以外は編集できません")
		tx.Rollback()
		return
	}

	seller := User{}
	err = tx.Get(&seller, "SELECT * FROM `users` WHERE `id` = ? FOR UPDATE", user.ID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "user not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	now := time.Now()
	// last_bump + 3s > now
	if seller.LastBump.Add(BumpChargeSeconds).After(now) {
		outputErrorMsg(w, http.StatusForbidden, "Bump not allowed")
		tx.Rollback()
		return
	}

	_, err = tx.Exec("UPDATE `items` SET `created_at`=?, `updated_at`=? WHERE id=?",
		now,
		now,
		targetItem.ID,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	_, err = tx.Exec("UPDATE `users` SET `last_bump`=? WHERE id=?",
		now,
		seller.ID,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	err = tx.Get(&targetItem, "SELECT * FROM `items` WHERE `id` = ?", itemID)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(&resItemEdit{
		ItemID:        targetItem.ID,
		ItemPrice:     targetItem.Price,
		ItemCreatedAt: targetItem.CreatedAt.Unix(),
		ItemUpdatedAt: targetItem.UpdatedAt.Unix(),
	})
}

type SyncMap struct {
	sm sync.Map
}

func (m *SyncMap) Load(key string) (string, bool) {
	val, ok := m.sm.Load(key)
	if !ok {
		return "", false
	}
	return val.(string), true
}
func (m *SyncMap) Store(key, value string) {
	m.sm.Store(key, value)
}
func NewSyncMap() *SyncMap {
	return &SyncMap{}
}

var accountNameToEncryptPasswordMap = NewSyncMap()

func setUpAccountNameToEncryptPasswordMap() {
	// : そもそもログイン前にキャッシュ可能
	// NOTE: これは平文ではない
	accountNameToEncryptPasswordMap.Store("nagatomo_shigeki", "dHdBKK1RRW6Bc4bJ8W2UyUkPxgX5HbDr2Ku3kXXHhqh")
	accountNameToEncryptPasswordMap.Store("ishiyama_erika", "rJ6lNcyuSy{j2Ulx:GqMkeOKvtgvyo,oOpqTl7rlz4N")
	accountNameToEncryptPasswordMap.Store("tomosaka_shunji", "un2x0IftPt:BoieHqTLDnMVbjzyYyHcCcJMSZut{jyR")
	accountNameToEncryptPasswordMap.Store("shirakawa_koutarou", "C7H54Kh,QmKpLnCk7FBMkRhh{oX[QOsLqQIvdbXRgWt")
	accountNameToEncryptPasswordMap.Store("misawa_katsumi", "xiJTTt2eyDg:TgNJpUP{VW{wZ36LbPhb{0J8:F1,X6R")
	accountNameToEncryptPasswordMap.Store("kubota_asahi", "{vFKe1BfTvBGcU9Znel6by:KMHyd,mC5ReeCCjR9o{9")
	accountNameToEncryptPasswordMap.Store("ichihara_koutarou", "CXybiTMEGsm9kTg0,3RrUy94fOXY0kH4,fr1IeWy75F")
	accountNameToEncryptPasswordMap.Store("seto_nagisa", "TQzqNqgSmymbbnBPZ[:LSRF4{PyzyMNt8EERbrMlJLZ")
	accountNameToEncryptPasswordMap.Store("shiotani_somegorou", "ntoJm0Glzlyn,UfRfekb0G2p1tZquR0hVUmvivw{ifB")
	accountNameToEncryptPasswordMap.Store("shimamoto_natsumi", "cDjsrgi6,c3CRT9umj[EffwvVsnrd8lSIsjvhBGRSnd")
	accountNameToEncryptPasswordMap.Store("shiina_atsushi", "vh5quCwE9vMZdyf,EuQ,M2LK,KoMu{Iq5gREv9Qggsp")
	accountNameToEncryptPasswordMap.Store("kita_mayuko", "7XMvNT1yi03wCVyLC5PU7RwZkv3bgNZg4P375hjsIS9")
	accountNameToEncryptPasswordMap.Store("takizawa_chise", "XrflX9W63ihBDN9uLNdGdNpFmCbqy[YbNmC[YNu1MPB")
	accountNameToEncryptPasswordMap.Store("asou_satomi", "fBz1LSORJcVjOR08eLK[6tJ7ECOzy5jhP[TnpLslzNZ")
	accountNameToEncryptPasswordMap.Store("fukada_akira", "7RfcMumEltpOwzq8P2q:0BpjO{[PzWeB6uQZpDtF:xR")
	accountNameToEncryptPasswordMap.Store("taguchi_hitoshi", "8N3No{Lnje4hF6fX1R{B3P6e1,QGN{V[hnPdvEYep45")
	accountNameToEncryptPasswordMap.Store("tabuchi_ann", "{HLScLtTd:wZDmrPM0uNyP4VyM4jnsTu6whBOb73wZh")
	accountNameToEncryptPasswordMap.Store("nakahara_erika", ":yVzuGNT2TtCedPKk4R3z{nImbrSvokH7M{oi:ih,yJ")
	accountNameToEncryptPasswordMap.Store("kashiwara_hiroyuki", "n609vSQq[qtinwiCrcLJ{CGwV[JcKCs:vPogrJuHHKN")
	accountNameToEncryptPasswordMap.Store("annzai_mayuko", ":XLQYcoE[jrVT[SzMPLwGC4rzy83OSIB{Ui[89XnUR9")
	accountNameToEncryptPasswordMap.Store("nagasawa_kogan", "z63IEBdfVw,uEy:0k97MshBXVT3mE4X7dTfuncL7PGt")
	accountNameToEncryptPasswordMap.Store("suga_tooru", "45V9uLFZsR{nrwq9evI8x4KzC7iP{hhcxL{1QjprvXx")
	accountNameToEncryptPasswordMap.Store("hosoyama_asami", "l2[HsOt6QS2OFgugkgnlbokirDYHpJEWKBvxHtFh1x1")
	accountNameToEncryptPasswordMap.Store("kodaka_rina", "b02FXHGTiQPwPXs37YftvpHV74C9YQ9KQqwJY1mZbqF")
	accountNameToEncryptPasswordMap.Store("mizoguchi_hidetsugu", "1BH6qp4DCUBmzfcxR74Pih1j,ryGpsTt69CmB[E35[B")
	accountNameToEncryptPasswordMap.Store("matsuura_ryuunosuke", "Ls:1MSH1s1MBSZElwkZxX[lJF1pMwvo6znsOL6Jmw4R")
	accountNameToEncryptPasswordMap.Store("morikawa_chise", "FYZVrVomuRHVCJctU0,FdCgnXTqZwB{7coht15rFsfR")
	accountNameToEncryptPasswordMap.Store("kadota_haruka", "UKic1pSvkmVm9O6zyCm,BwfZzuqLdpKlFLWoZF{eXxJ")
	accountNameToEncryptPasswordMap.Store("yamashiro_mantarou", "lBHeXyuUj{gGzEDGN,UHTmNKxpDvseP8W:e{gJzw[2R")
	accountNameToEncryptPasswordMap.Store("kanashiro_satoshi", "3T9ECcpbNNopelDe,xxrdJ7VhF{Wtg2z3Nghw38T7iZ")
	accountNameToEncryptPasswordMap.Store("ando_mitsuru", "gIWv8{etUlefJuOv9vno[Ctot74l2O{lbUCWOk2:By9")
	accountNameToEncryptPasswordMap.Store("yoshizawa_tarou", "k9c5MUfU4n:FERkU5tBoUWJ3nC,HGj7iNh3LI3LePZV")
	accountNameToEncryptPasswordMap.Store("tsukahara_yuu", "SEHN4EU[2nkytfmcc0XNQNbo6ry,7ZBp7HLdeLPwh0V")
	accountNameToEncryptPasswordMap.Store("hanada_emi", "MVwk4JY30DccnjWPqwn7USYLj[y6Sw6CV2ZXJrCk8Jl")
	accountNameToEncryptPasswordMap.Store("oguchi_stmaria", "Wn0VtX2N64J2rSwOf6XhczgC62gf[rx3wXPBrO7jK,x")
	accountNameToEncryptPasswordMap.Store("usui_somegorou", "qU{5XioiNpMg7XtOFNWh,CCZNYd1YBLPgcDHYPedbQt")
	accountNameToEncryptPasswordMap.Store("iguchi_atsuko", "q2Ul[EEsG{MoGvHFpBPjYbGVu,2VCQGdQx3ZWQbwdLl")
	accountNameToEncryptPasswordMap.Store("takigawa_nana", "tfZS{[XIXRQTbNuIovkxSPH2n1xXEX2P[:x1o:xbupJ")
	accountNameToEncryptPasswordMap.Store("akagi_shunji", "ygqIQUSOpiDgGHfVGzl,88:LCXY2iRsrGxtwO4QEQ35")
	accountNameToEncryptPasswordMap.Store("taguchi_ikue", "rCTdYnL4pFLwP2MsdJzPDb3bczDloEGUt4uonXH7Mz5")
	accountNameToEncryptPasswordMap.Store("shiratori_akane", "WfkeJy0r1,f:hRqHO1X5,sRD09U8:8iGd8gYbOVy24B")
	accountNameToEncryptPasswordMap.Store("hanaoka_kazuhisa", "sJuxl86QpR,NyrTLUlpBmIuLw{nm,3:[KYm:gu3:lI5")
	accountNameToEncryptPasswordMap.Store("tachibana_masami", "5HSqEN9mbzzZfZ1xXwiunPbeHyrHmZxKsRbWTKkYppZ")
	accountNameToEncryptPasswordMap.Store("morimoto_kazuya", "{UQf,n[jpJsYZ7YyONjTix8PxmyKztPNcOg[IYxys[F")
	accountNameToEncryptPasswordMap.Store("matsuda_teppei", "EhVYXnw1olPP{CKsyCUPi8tBqgepk0QntgDP8IOuK3V")
	accountNameToEncryptPasswordMap.Store("kamiyama_yuu", "ZSnKyFlpIOtN1K[o{hQz3ZCJB8Gn{q,hqgKErzYmtBx")
	accountNameToEncryptPasswordMap.Store("kozeki_hiroki", "tnu[ftY:s24ogQM[bKqq[qp9gEWV15INQdwP8T[Q0n1")
	accountNameToEncryptPasswordMap.Store("miyabe_hanako", "ItvrPVlL{kC6r2kyJdEsQ,HsHx3iJurFvjCYtNpTjv5")
	accountNameToEncryptPasswordMap.Store("isaki_youko", "e6Ln6DqODpqjSqTMZkfkmwHqB6j79[QCrL7TBSmqO0V")
	accountNameToEncryptPasswordMap.Store("kawano_saya", "7w0Lh31DFZ1W0nSXd7,uCcYdZ8[[oEX{QUuGt{BJDdl")
	accountNameToEncryptPasswordMap.Store("nanase_hiromi", "lp3Sg2vp7xc3oP9Q3GTpbRusdp1s:1y9LJ,Ny6sTx[9")
	accountNameToEncryptPasswordMap.Store("teramoto_maho", "lwhcEpusFfUlPS4s6nsQF8nmG3JSTtnGhF6uEBhW07h")
	accountNameToEncryptPasswordMap.Store("banntou_toshie", "7Tuhl1oz0Mfc2LX9iDXyTz[P9Yl8UzengbjXp:dIUe1")
	accountNameToEncryptPasswordMap.Store("takahara_misa", "IxlTf{2Jzqe4FpWwS1liO6Ti[M,Ol,dBcZNSDrnpuyF")
	accountNameToEncryptPasswordMap.Store("tanaka_keita", "WKvXDotlIQR40{{0638V{6w,xfEWQX5:,WwSnNVZP0p")
	accountNameToEncryptPasswordMap.Store("deguchi_keiko", "0BdC,4N4yu0p[8ZG29ZzYN:SG6s,qEpKH93rIqyJykB")
	accountNameToEncryptPasswordMap.Store("miyoshi_koutarou", "ccSlz,ml,3{ecVK7KC55hIfJroM,vGFVCcs,1Qs6zql")
	accountNameToEncryptPasswordMap.Store("furusawa_chise", "Nzt[6[:I:WW0cuEyHgB2g,DpIqK3CNRllMMOjkTd:XJ")
	accountNameToEncryptPasswordMap.Store("nakagawa_mamoru", "DrJzeeFcC{xbBhKi1ODjlJjO8fb,JcINBwFRJx49,ZN")
	accountNameToEncryptPasswordMap.Store("sugino_aki", "W:Fhvq7CQd0fTLCPXdMBrqMsToUuUmtX[xrsf4Szq5R")
	accountNameToEncryptPasswordMap.Store("hori_miki", "v2pJmw4fS5j4D6EPJC5Wd5yOggpLyyNMfzLRtp8x:U9")
	accountNameToEncryptPasswordMap.Store("mizushima_hideki", "uhzFQUwOO8R,ZodWUV9xejDTsixIiP13Fb17x15Cd7t")
	accountNameToEncryptPasswordMap.Store("onoda_shihori", "i{jKyP4:o2pIQNbdCq3veHlQ3qqECWMbEk24wNydhsd")
	accountNameToEncryptPasswordMap.Store("kojima_emi", "hmwhxIwQbbkNttC3tskYnTgJpxnv2cFmtHqItWKd{gd")
	accountNameToEncryptPasswordMap.Store("kuramoto_takashi", "XDFz3L4kfkfXb7y0mCys0TL0uuQ8YVD:ZIjNr7IqIQV")
	accountNameToEncryptPasswordMap.Store("kobayashi_atsushi", "m[VpvlZBiThWMEGsCBVjEvDqBTBKE{64WC{nKhHQ65R")
	accountNameToEncryptPasswordMap.Store("nakane_kenji", "[FBo0b0Hd8h:YwCM2BZlYJuX3rPduLTLT:HtLj:KgOR")
	accountNameToEncryptPasswordMap.Store("taniguchi_tatsuya", "c{zT3[{JeorXItG4T1[dhVVUcFxt6pbmLCPDb[2BFTx")
	accountNameToEncryptPasswordMap.Store("nakagawa_manami", ",yypdYC3lqUH3pveMmufQDZ1wrpvfIeJ2iPuVOTg5Bt")
	accountNameToEncryptPasswordMap.Store("ichikawa_megumi", "3IV6R{6jZCsFfwbb6lFN7y5Tx:Z08kcgL2OlSX8S{uV")
	accountNameToEncryptPasswordMap.Store("take_kanji", "rsx[7V7{p22Xgylmcnt{5gRK49v91X7rnbu69tX5eEh")
	accountNameToEncryptPasswordMap.Store("tokushige_aki", "cJITNd:T[UyiJF9HZQbK:ybV8hL46zBX0bpMbswVGmp")
	accountNameToEncryptPasswordMap.Store("fujisawa_naoto", "16XNJhFjXjtGUuUSYF5UiwHDZ8ZTKHnLZhfNMvLluSJ")
	accountNameToEncryptPasswordMap.Store("hamaguchi_masahiko", "s5OII68zZ8UYtFxgUZ78Q3pq3Yevi8PWudZT1GBTMoJ")
	accountNameToEncryptPasswordMap.Store("kanai_ikue", "z1UCYB[MrIQYbiReqRuvPvCC51piY21H7trrmLJ78Q9")
	accountNameToEncryptPasswordMap.Store("nakano_chise", "K3nQveGjWDN9pPyVXv[ddgLZx3fxbk7ww,KUYqeYuIR")
	accountNameToEncryptPasswordMap.Store("matsuoka_misako", "Wzf4b[dccd3[WTLfPfMWsQceq3h0vmqrm{G0{QK4Ond")
	accountNameToEncryptPasswordMap.Store("takaya_shiori", "wdb6ooni24,Ie,,Xkd{[I2[omGDSlNZpH73:jJ,nTB9")
	accountNameToEncryptPasswordMap.Store("ueno_shingo", "{JUs:DoV5bzCQrcEG{[Ihowru,PF4:IKLLDi6zLVSz5")
	accountNameToEncryptPasswordMap.Store("fujishima_misako", "1ZgPH2SoP[Kzq,Yl2vfngIQZphdQy[WL3hwno{NIGDl")
	accountNameToEncryptPasswordMap.Store("sagawa_yuusuke", "cO{zfLnQRhtlnkRC31rf[kz:4HryOxdEggw9pN,:QzR")
	accountNameToEncryptPasswordMap.Store("okumura_ayame", "QhjEIphbHXf79R81FI3UcJE9iDoRonLhnVb6,EtHJGR")
	accountNameToEncryptPasswordMap.Store("nagai_miwako", "TcgioIuPdGRXyM8GhYHeogUftsJnWOshxfSnoJq6rfR")
	accountNameToEncryptPasswordMap.Store("yoneyama_miyuki", "6I[vPG35fp5E[SsZRIn6Mh9UhZ1cEl[CxT4y2u{FI2J")
	accountNameToEncryptPasswordMap.Store("iwamura_megumi", "il[{SstJ6xcZhkTq91[XoIH5c9Wf[er4CGr8Ipm,4ul")
	accountNameToEncryptPasswordMap.Store("araki_atsuko", "EH{xErBnu95buvGoPUrXdj60zK68Dux6WZVzCLPUy:h")
	accountNameToEncryptPasswordMap.Store("mikami_kimimaro", "HlCuhKscXqmcS:3MiMjG6lv[ijmLsnmqMWt56YpXDSR")
	accountNameToEncryptPasswordMap.Store("nakano_risa", "H7T4O6eL6qw0PHIgRd{5uGponp,iF,ElwzRVrysm79J")
	accountNameToEncryptPasswordMap.Store("irie_shou", "nj1IBgBVQUXy1efyKTzwHNkDpsoJHxpHhV5do7sTh4V")
	accountNameToEncryptPasswordMap.Store("nagasaki_michiko", "rxeFeWVgm6QFScL49LRlPhvQdVfX[ZeD,whcQkcEy4N")
	accountNameToEncryptPasswordMap.Store("sugimura_atsushi", "TK2Fy9dLNfz8z7BKyhgMec7KGqqzJXfvRG3oWnGKuHx")
	accountNameToEncryptPasswordMap.Store("shibuya_ryousuke", "XrwXFETyk5Nn1{,Zqqrf8GJNbB81SQ,FKD2MqMH{fL9")
	accountNameToEncryptPasswordMap.Store("tashiro_katsumi", "U9:sQoW,oWulkuVfWxo0GD49OXBNSY3bg,s6inO2[SJ")
	accountNameToEncryptPasswordMap.Store("aiba_megumi", "9gdQm2e4lMdLR4jhZ[1n1oEi2STqXymW81c[[h7sDOt")
	accountNameToEncryptPasswordMap.Store("mizoguchi_kogan", "13,{unQiw8FmWDEu6iITT1RSDx26mXKN2wGfFpTedfV")
	accountNameToEncryptPasswordMap.Store("nakano_sachie", "F8LmjVEUDZGi2Kvu95Vp61sTtDpLN8cVcx20u9ZR4Wh")
	accountNameToEncryptPasswordMap.Store("nishii_masahiko", "9SXV0MkFP4slPDwXYqqvnE{SP{cp7QLCi4FbyX0G0zJ")
	accountNameToEncryptPasswordMap.Store("takagi_kouji", "tBRbgoGNXXoLiSrGBHgEZJyxDHX6Q0fIjERLsU0fTxd")
	accountNameToEncryptPasswordMap.Store("seo_natsumi", "H6vRoq[t4[e5bV{tTQSE3pLydS2itxHmdK5lTUx8eCx")
	accountNameToEncryptPasswordMap.Store("yamane_yumiko", "bjzQKnBWKGwsu9HUN5[Y1Njz1myNSWfG376chgxm[pF")
	accountNameToEncryptPasswordMap.Store("teraoka_natsuki", "UBuQXRXTPh3ZhRqJgMjtdoqHKlBF{eFY7myVy90kj5R")
	accountNameToEncryptPasswordMap.Store("fujimori_masami", "lVTC35UYJIsZz8lSyWUwmyjyNk3qboLmtUbmYkZ8bId")
	accountNameToEncryptPasswordMap.Store("oonuma_george", "QnwVJXF6UeFjupe7TMjP0koKDuRVsfeX53HRWtO0HQd")
	accountNameToEncryptPasswordMap.Store("kase_hikari", "3LY0q62gviW3c,psyGIHYctCRO7LIpQ6FxsJOCE1N,B")
	accountNameToEncryptPasswordMap.Store("tsuchida_myuu", "SG2xZ6mELExc1d:T[{9u{4ON:EUFdTn0yw{zS6MVVSt")
	accountNameToEncryptPasswordMap.Store("katsumata_hikaru", "8bp3ISG9uS,XH{f5bEny:,b7SwGu,pGE9[wLv2m3ZwB")
	accountNameToEncryptPasswordMap.Store("sugai_satoshi", "Sq:OgbY:ceveOv3s1,e4v4t0EnQeexXcW5VnLn[RpCp")
	accountNameToEncryptPasswordMap.Store("okuyama_tooru", "{733VCiv50e8T3DOpkjuOheQUdoU7jrNdwpHHHUngMZ")
	accountNameToEncryptPasswordMap.Store("take_souta", "7HOGoqwN81ZZYIo1N1p2D4dzjd5ToeXEmodIbSk:7Wd")
	accountNameToEncryptPasswordMap.Store("takami_shidou", "DzRPm9YH7jsKB5QR:819o3q6wD:Hfftykru7PhFrJk1")
	accountNameToEncryptPasswordMap.Store("hinoshita_hideki", "KKS7E6zTvDVF[JkoRPBVTY8VPvuLzjLcUucNQT82mVV")
	accountNameToEncryptPasswordMap.Store("iwai_teruo", "H9dpo1YB0owWjrtypEyKULS7y0GEBxBqhi[xT5q56cF")
	accountNameToEncryptPasswordMap.Store("yokota_mina", "vD:86p1MmNs7c[Dve0CpOnk[yrGHp6Vz[yEvnDKIOZh")
	accountNameToEncryptPasswordMap.Store("miyake_asami", "2{oLX{vIVp[rbrdoY2zG09nD3Ms4CYE0pUq7ptIXWqB")
	accountNameToEncryptPasswordMap.Store("utada_tetsuji", "6Z5OMkI4LsH5ZesR[yYu4PDE{HY0Z,Z:vlK,:UhT4CJ")
	accountNameToEncryptPasswordMap.Store("tsuchiya_meisa", "2kMsiEyQqzHW27QPpTRxI:T0j,hgVS04RR4pPt7bPKd")
	accountNameToEncryptPasswordMap.Store("sakai_nao", "C4VWTlb7in2,cLMRIV[RBdTy:Lh6X[3{yTpRVhFED6R")
	accountNameToEncryptPasswordMap.Store("oguri_yousuke", "YMq3BHCv8QXiThVl{g8P[t0b[m7cwT2LHcbobiHmPqR")
	accountNameToEncryptPasswordMap.Store("sakai_asahi", "1KPURdXeq{rg,,ween462gRQBELMNC8m2:wPvkWFcdZ")
	accountNameToEncryptPasswordMap.Store("sugino_toshiya", "jhyyw5ekiC0NQsItR4YnQkuTI3T4uMQIdvlKFTQwBUV")
	accountNameToEncryptPasswordMap.Store("kuruma_maki", ",ODW{sRUwtTdLMhnGHke[EzXqDd73{C:{twvxfM,Xo1")
	accountNameToEncryptPasswordMap.Store("shiratori_shinichi", "XprW5YN3LSn713jZi7kyyzEg9{wsgbKS7X:{47hWuEp")
	accountNameToEncryptPasswordMap.Store("matsuda_youko", "yOpOmQIWB,xEKcXFo7nP,kKIpzEKmJTXSODROuG6IbB")
	accountNameToEncryptPasswordMap.Store("aizawa_akira", "eql9pv[0n,Xzvk93RhruUlb:Xs,[9YZ1GNVTl0M{vhJ")
	accountNameToEncryptPasswordMap.Store("shinagawa_akane", "txI3rZ0drce{Z8H[B1gbZHdHFIC4FyBfyr7Y3O7WLsl")
	accountNameToEncryptPasswordMap.Store("kanai_hiroko", "g4NblJdUMWhegiMIWTW3X1ox7U7yyD9G:9CIeDDpkkV")
	accountNameToEncryptPasswordMap.Store("kuwata_kimiaki", "hFB:zMgplbeoTY{BQ,CWTNYnTIdDSimE86oxb:c{{N1")
	accountNameToEncryptPasswordMap.Store("yatsuda_kousuke", "Ih8pwKRK1VyPgPtN[DoIeJXt:76CViJ[4SPBqdp4e1h")
	accountNameToEncryptPasswordMap.Store("miyashita_hajime", "[pc[JWskpf,XiW2L7j8diG,:CwBPkbIVGr3ZrpRktFh")
	accountNameToEncryptPasswordMap.Store("mizoguchi_nao", ",jmShhsTV6w5CvrPe0BdD6JEGLLUvT[k,KKm2[ZZgNR")
	accountNameToEncryptPasswordMap.Store("watanabe_kanji", "ovTsnDpe:J9Gp1CmlEleksepDT:vRddP3{Y{ec4:{MB")
	accountNameToEncryptPasswordMap.Store("matsuzawa_masayuki", "JpeBDobt6VLbYHYr2RcUEXGE5h49N8KE4NZD:ZGU59p")
	accountNameToEncryptPasswordMap.Store("hosono_yuu", "uNiF2RVVDoNqn9mISElUsgXNE3G79sk00P1B5n95101")
	accountNameToEncryptPasswordMap.Store("shiraishi_masaaki", "NMqn8xJOjvcByzz8sChqiHpGPfTkp1WosJyv{R52{X9")
	accountNameToEncryptPasswordMap.Store("shouji_kenichi", "Ge0it,{Zuu3dn:foZg36g0GFS0jecrmF373grdss4:1")
	accountNameToEncryptPasswordMap.Store("yasoda_myuu", "bgSNEJsxEs40QCtw[F{[bpPe7JyO:2gFFefckZ1XjfN")
	accountNameToEncryptPasswordMap.Store("kitakawa_nagatoshi", "H:HuZvZzo6EWlgmt7MglIn9J:C:CWZj{v[6:j{x11kJ")
	accountNameToEncryptPasswordMap.Store("togashi_mao", "wTi076ed1kkQc1Jf0HT498o99CElwFRuLxH8Qn0e:Xp")
	accountNameToEncryptPasswordMap.Store("chiba_ryou", "RBs3C7vJLQe:QSdBILQX1NITz94eGsfETj5Lc2n7Yj5")
	accountNameToEncryptPasswordMap.Store("kawamura_seiichi", "bRt6WL6cvUJ8Cx:j3yhcLKTZJf0B9gSptsgVueLY7,x")
	accountNameToEncryptPasswordMap.Store("takemoto_hiromasa", "DUPOQvHbg51N1isFZ4DsPzitMjg8QNCRjeC:pieohXF")
	accountNameToEncryptPasswordMap.Store("nogiwa_shunji", "wpnvnXuerk1HtI6Usg4sVyLleYr29EQQbnT,jgSDRsB")
	accountNameToEncryptPasswordMap.Store("hirasawa_ryousuke", "ITx:VGs:X:qWvsmPZ,wS3UXr:EFzWVvBj38XM2,[64F")
	accountNameToEncryptPasswordMap.Store("ookouchi_miwako", "gFdbuLn14OiP8NNpq7r9D9wbm1Bje14LEXGUo6y9IuV")
	accountNameToEncryptPasswordMap.Store("shiina_takahiro", "15ytzqI1d3qPi3yXjccBlPtnt{xm6FY5rp{Bsdrw[Sp")
	accountNameToEncryptPasswordMap.Store("takiguchi_masahiko", "eyWEvNU8cphHe2elzunQzyVxUoel[XfJLHsKgJSk3jh")
	accountNameToEncryptPasswordMap.Store("toda_ukyou", "GCi[sxnXBfq,61LooBNkQqExZXoc0O8D32y8{OMr4Jd")
	accountNameToEncryptPasswordMap.Store("furutani_masatoshi", "PztbF:S1pYQW{kR2CLn:fRO3t6u{ZpxyD:fFGJIVnPl")
	accountNameToEncryptPasswordMap.Store("arima_reiko", "sYh7[jD4t9ipK7HSF1v5jiUJNTRN0eQ1VoH3u2yg:xt")
	accountNameToEncryptPasswordMap.Store("nohara_asaka", "Wwfuejr{f7YTzPHJJc5rbZ61lelBEizMhPCSxD:Sg3h")
	accountNameToEncryptPasswordMap.Store("kasuga_tetsuji", "jKz5zxloDpi9lJs155LBBrP5CQ0Vq0IikMT3MBeD0Z5")
	accountNameToEncryptPasswordMap.Store("kokubunn_mia", "e,x[iqpnFvB3SsFmuWG9ZgFCOb5kCTDx59fzfriNOwx")
	accountNameToEncryptPasswordMap.Store("niimura_megumi", ":9xGqvwBxDKZXZvz0FtjBGjVxLp0xHYEpos{0IEp3UR")
	accountNameToEncryptPasswordMap.Store("ochiai_kaoru", "KFJfV{0bzoHkoDjddmxGTPpW51kqrvBu,joS1s7eeCV")
	accountNameToEncryptPasswordMap.Store("matsushita_takane", "BRBURRruHPWF15E4xD8rHXyzSlhIKLbxUeVMwT[bc01")
	accountNameToEncryptPasswordMap.Store("matsuda_kogan", "LWvd:DIbGR6XHtYRdGfEWjJ0,hsSPuzK0GW7[0x837x")
	accountNameToEncryptPasswordMap.Store("miyake_naoto", "N2J9{Iu8dtqyG9KcZj:JlId[JnD5kJMldCFtfSsfyNR")
	accountNameToEncryptPasswordMap.Store("asakura_rokurou", "Z6i07oB73:WGNDK1fQ9xuFv{fNJJzbg2D[sfpC9R4c5")
	accountNameToEncryptPasswordMap.Store("fukiishi_seiji", "1j{ckTsDK:KL,8BsyYeu3UqtN6WJEtmr3OyP1Tlfy9h")
	accountNameToEncryptPasswordMap.Store("nagai_hana", "{x6BEmIbV:LY:2DIV{PWMryTtRicg4KcWC8xbiiGzSd")
	accountNameToEncryptPasswordMap.Store("miki_nagisa", "MeDEG[{[rTqkM9g1gNuRz7czMz,3V1tyvR::CmVgHT5")
	accountNameToEncryptPasswordMap.Store("okayama_nagatoshi", "Q2S7luGCWDUr7JzjQHBF98UjMhjFLMLyrDS4EslP9[F")
	accountNameToEncryptPasswordMap.Store("sawa_shouko", "IbumbVISKMbFNzIcTwUvcw[ZugtFmGCJkSxfEuOHi1R")
	accountNameToEncryptPasswordMap.Store("akasaka_meisa", "udJDK9w[{U[C7wevELUY1{dsWPwHwX9EEo,x093t3pB")
	accountNameToEncryptPasswordMap.Store("tanabe_airi", "MCqUs7EsT2dCsuVZbjB1vbHVBhch4ocvO5eeBs{jPlR")
	accountNameToEncryptPasswordMap.Store("fukushi_nobuhiko", "ZZhV9kj8fTWfldBII8iueQtWFQGRhsXz6pjOz4Pz0Jd")
	accountNameToEncryptPasswordMap.Store("nagasawa_george", "q4kLcVd33I1,:[cWsFoyIr6EqT5m{ZK,Zq5SkjYTwKx")
	accountNameToEncryptPasswordMap.Store("niimura_keiji", "FGZb,:Ub8LJ4QOZrhg8zO1xUItrKNnozpLBJWB2ty6F")
	accountNameToEncryptPasswordMap.Store("kashiwagi_mahiru", "lY,pKtLOyr0D:fJeF9HB9vuY:DRKRzmgDHCFtTb8Oq9")
	accountNameToEncryptPasswordMap.Store("oosaka_tatsuya", "FNnBIhgVWf,N4ko4YrEtc8ZjbzOL3C3xxnuu97bnfmJ")
	accountNameToEncryptPasswordMap.Store("shaku_tetsuhiro", "VqltxIO6IhhIloQ,,Wfg0fNHxels9m350gwT:7{PjCV")
	accountNameToEncryptPasswordMap.Store("nanase_kenichi", "ELto:mdiJZ[U438SoRLvpZo[,XHBw8d{Ei2M3St[USx")
	accountNameToEncryptPasswordMap.Store("taniguchi_hironari", "IJq8eFPLUC9vYi[4zsCpV5GDf3rnBIbEuxwJVdJN3i1")
	accountNameToEncryptPasswordMap.Store("arita_emi", "1XI8Q7ZT0udNPYFWh6{{[qjPhziHXIlYXYyIswh4mLZ")
	accountNameToEncryptPasswordMap.Store("nakayama_daigorou", "prDhTS7MTESIzh9KXEpwgpTYZCsuWLn22ImmFm3R9GZ")
	accountNameToEncryptPasswordMap.Store("kuruma_yuu", "py9RQWgoKpZg6rJJJ[rv7:eL6UNsMwQcS,j7vcEXx3Z")
	accountNameToEncryptPasswordMap.Store("ooura_masayo", "t8fIlBU18{bXcX9HWVqc8{qkT87Y5wKlpxX,PV6NwZ5")
	accountNameToEncryptPasswordMap.Store("nannbu_tatsuya", "gMHeRcYdm8x{{WpbWsxERG,G4DG6VZtYJVNOzqkKK[V")
	accountNameToEncryptPasswordMap.Store("umeda_akane", "INmniw3JCKmMBN[keOG[dzPJqnmIBLDTeqLSGJ9t1np")
	accountNameToEncryptPasswordMap.Store("nishioka_mina", "Wx7PHhu[G1dYHSKghH{k6sR[x,0DtBhlsmESofzYmKR")
	accountNameToEncryptPasswordMap.Store("okumura_ai", "S7EYx0xzY25LTMexl9F39[toDsT6b5IV58P3LMFrkct")
	accountNameToEncryptPasswordMap.Store("fukawa_nana", "OE,T{odhVBr0eqngrx[RPBtdhFXOwt:HYPXBR,:Y7bR")
	accountNameToEncryptPasswordMap.Store("seo_yuujirou", "sUVRP5loE0XT9LLtgcG:CsLlCNs85WE6il:oW792GiN")
	accountNameToEncryptPasswordMap.Store("mitsushima_mia", "WmHjKmc88yWudGlUtYBZpMhKTWSRuyBMohEOTbnjjV5")
	accountNameToEncryptPasswordMap.Store("konuma_mina", "76ku4REQuWv,F6tIs7TJ6qIc6Qe,xyKEsy67KoUFScd")
	accountNameToEncryptPasswordMap.Store("hori_misako", "RISjhVFeX01NQNOXU9WFQEyn:VlX8SLKZs:v8LYOdE1")
	accountNameToEncryptPasswordMap.Store("akashiya_masaaki", "y:ciSURImKBwZw0nztO0c{qN3uf27bQRBkx4uKM8[bV")
	accountNameToEncryptPasswordMap.Store("hirara_subaru", "di6jNo5{6o7LsiW4y7hc7Nkes9L1QnkDXc3SrE62kel")
	accountNameToEncryptPasswordMap.Store("teramoto_sousuke", "nmTFmseKIgmlqFbmBFmIqo3K52htGwWZ{XGzvl{9Vul")
	accountNameToEncryptPasswordMap.Store("kai_shinya", "Mg0uH:sxi9zcRYYOXcO{V[ePkFOjPV88zZD::UbcrvB")
	accountNameToEncryptPasswordMap.Store("sekine_ryuunosuke", "eZCN8clPthnb7BpNVycHeQFUsb3d20czn:mPPo5[c2l")
	accountNameToEncryptPasswordMap.Store("tannba_hitomi", "i[XTpRYuTobm[lhKBRIcLFLw3XUvZcu6OBbLkJ4C7rV")
	accountNameToEncryptPasswordMap.Store("kinoshita_nozomi", "1bpkTcCXBCvsf4MCxv[4MDC7I7NzgNsXN[UzRMfLChV")
	accountNameToEncryptPasswordMap.Store("furutani_kouji", "Udjv87QZE3g{GiigNW3lyfyN4cIeFb7ibzz:DYXY{{1")
	accountNameToEncryptPasswordMap.Store("sudou_hikari", "JC{mi5BycqUMD14k9LtedkOq2khDE0VxGKIRTu[LNu1")
	accountNameToEncryptPasswordMap.Store("tsushima_ai", "K[evx6m,MTeFqOp6JMwKoPsOcWZBfHnsrj4FcBufQtF")
	accountNameToEncryptPasswordMap.Store("izawa_reiko", ":FGLJsZ968ULXWXXlTJ,wv8Jy4yG271uNILJLJigefV")
	accountNameToEncryptPasswordMap.Store("hori_keisuke", "zKjuTOs[HwXmCoURlD5GEz8TLFb330EkU4LdrtG1qZR")
	accountNameToEncryptPasswordMap.Store("ono_reina", "U{1S{,ut7qry36f3BvRpGEm7WJSxUuMPX:m0pcmXQ:J")
	accountNameToEncryptPasswordMap.Store("shiratori_riko", "b2kf,sx2KhPY2dU8vn:nX:ggnsS1{b0tcI9zXQp8Zq9")
	accountNameToEncryptPasswordMap.Store("ootomo_risa", "q89W33msEfKJqvcRuTJCgW5e2,3vNd[d3C8J5SB761t")
	accountNameToEncryptPasswordMap.Store("ooji_yuu", "mCR[qeQeX,hI9Ws2qWL:8KCK8hB,pfNImZTkfkUbZe5")
	accountNameToEncryptPasswordMap.Store("shinagawa_kaoru", "VXUDxMCO0j7B6BMwrnUO{lUNmosDCUWic6KIJ{4ST55")
	accountNameToEncryptPasswordMap.Store("hirosue_aya", "3BMYnGmw{8,,ZM5C3O9MtNBE2RObTz90,LOkcXs5,rJ")
	accountNameToEncryptPasswordMap.Store("kawano_natsumi", "QO8CW3zE8sMlviFpGNllnSkvk6EY3plnGIuloYP6Om5")
	accountNameToEncryptPasswordMap.Store("tachiishi_yuuko", "DEI:BpF09binHMHHQjBeNhHuL[:EF3y7jZe5lUdwkvR")
	accountNameToEncryptPasswordMap.Store("ayase_tomoka", "IZr3XBmbieegt0Iub4l2DdMGsOKkI7cBSjNVpKVk1jN")
}

func next1(s string) string {
	s2 := []byte(s)
	for i := range s2 {
		s2[i] = s2[i] + 1
	}
	return string(s2)
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
	u := User{}
	err = dbx.Get(&u, "SELECT * FROM `users` WHERE `account_name` = ?", accountName)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}
	next1Password := next1(password)
	if v, loginExists := accountNameToEncryptPasswordMap.Load(accountName); loginExists {
		if strings.Compare(next1Password, string(v)) != 0 { // パスワードが違った
			outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
			return
		}
	} else {
		err = bcrypt.CompareHashAndPassword(u.HashedPassword, []byte(password))
		if err == bcrypt.ErrMismatchedHashAndPassword {
			outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
			return
		}
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "crypt error")
			return
		}
		// 新たにログインに成功
		accountNameToEncryptPasswordMap.Store(accountName, next1Password)
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

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), 4)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "error")
		return
	}

	result, err := dbx.Exec("INSERT INTO `users` (`account_name`, `hashed_password`, `address`) VALUES (?, ?, ?)",
		accountName,
		hashedPassword,
		address,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	userID, err := result.LastInsertId()

	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	u := User{
		ID:          userID,
		AccountName: accountName,
		Address:     address,
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
