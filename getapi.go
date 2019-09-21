package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"goji.io/pat"
)

func getSession(r *http.Request) *sessions.Session {
	// これをキーにして返す
	cookie, err := r.Cookie("session_isucari")
	if err == nil {
		// csrf_token / user_id
		// fmt.Println("COOKIE:", cookie.Value)
		if val, ok := sessionCache.Load(cookie.Value); ok {
			res := val.(sessions.Session) // copy
			return &res
		} else {
			session, _ := store.Get(r, sessionName)
			sessionCache.Store(cookie.Value, *session)
			return session
		}
	} else {
		session, _ := store.Get(r, sessionName)
		return session
	}
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)

	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}

	return csrfToken.(string)
}

func getUser(r *http.Request) (user User, errCode int, errMsg string) {
	session := getSession(r)
	userID, ok := session.Values["user_id"]
	if !ok {
		return user, http.StatusNotFound, "no session"
	}

	userIDStr := strconv.Itoa(int(userID.(int64)))
	exists := idToUserServer.Get(userIDStr, &user)
	if !exists {
		return user, http.StatusNotFound, "user not found"
	}
	return user, http.StatusOK, ""
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "index.html", struct{}{})
}

func getNewItems(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var itemID int64
	var err error
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}
	// NOTE: カテゴリーごと ?
	// user, errCode, errMsg := getUser(r)
	// if errMsg != "" {
	// 	outputErrorMsg(w, errCode, errMsg)
	// 	return
	// }
	var categoryIDs []int
	categoriesExists := false // nil != dbx.Select(&categoryIDs, "SELECT category_id FROM `items` WHERE buyer_id=?", user.ID)
	items := []Item{}
	preQuery := "SELECT * FROM items WHERE status = ? "
	midQuery := " AND timedateid < ? "
	proQuery := " ORDER BY timedateid DESC LIMIT ? "
	timeDateId := time.Unix(createdAt, 0).Format("20060102150405") + fmt.Sprintf("%08d", itemID)
	if categoriesExists {
		categoryQuery := "AND category_id IN (?)"
		if itemID > 0 && createdAt > 0 { // paging
			err = dbx.Select(&items,
				preQuery+midQuery+categoryQuery+proQuery,
				ItemStatusOnSale,
				timeDateId,
				categoryIDs,
				ItemsPerPage+1,
			)
		} else { // 1st page
			err = dbx.Select(&items,
				preQuery+categoryQuery+proQuery,
				ItemStatusOnSale,
				categoryIDs,
				ItemsPerPage+1,
			)
		}
	} else {
		if itemID > 0 && createdAt > 0 { // paging
			err = dbx.Select(&items,
				preQuery+midQuery+proQuery,
				ItemStatusOnSale,
				timeDateId,
				ItemsPerPage+1,
			)
		} else { // 1st page
			err = dbx.Select(&items,
				preQuery+proQuery,
				ItemStatusOnSale,
				ItemsPerPage+1,
			)
		}
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		return
	}

	itemSimples := []ItemSimple{}
	for _, item := range items {
		var seller User
		sellerIDStr := strconv.Itoa(int(item.SellerID))
		idToUserServer.Get(sellerIDStr, &seller)
		category, err := getCategoryByID(dbx, item.CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			return
		}
		var simpleSeller UserSimple
		simpleSeller.ID = seller.ID
		simpleSeller.AccountName = seller.AccountName
		simpleSeller.NumSellItems = seller.NumSellItems
		itemSimples = append(itemSimples, ItemSimple{
			ID:         item.ID,
			SellerID:   item.SellerID,
			Seller:     &simpleSeller,
			Status:     item.Status,
			Name:       item.Name,
			Price:      item.Price,
			ImageURL:   getImageURL(item.ImageName),
			CategoryID: item.CategoryID,
			Category:   &category,
			CreatedAt:  item.CreatedAt.Unix(),
		})
	}

	hasNext := false
	if len(itemSimples) > ItemsPerPage {
		hasNext = true
		itemSimples = itemSimples[0:ItemsPerPage]
	}

	rni := resNewItems{
		Items:   itemSimples,
		HasNext: hasNext,
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(rni)
}

func getNewCategoryItems(w http.ResponseWriter, r *http.Request) {
	rootCategoryIDStr := pat.Param(r, "root_category_id")
	rootCategoryID, err := strconv.Atoi(rootCategoryIDStr)
	if err != nil || rootCategoryID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect category id")
		return
	}

	rootCategory, err := getCategoryByID(dbx, rootCategoryID)
	if err != nil || rootCategory.ParentID != 0 {
		outputErrorMsg(w, http.StatusNotFound, "category not found")
		return
	}

	categoryIDs := parentIdToId[rootCategory.ID]
	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var itemID int64
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}

	var inQuery string
	var inArgs []interface{}

	if itemID > 0 && createdAt > 0 {
		// paging
		inQuery, inArgs, err = sqlx.In("SELECT * FROM items WHERE status = ? AND category_id IN (?) AND timedateid < ? ORDER BY timedateid DESC LIMIT ?",
			ItemStatusOnSale,
			categoryIDs,
			time.Unix(createdAt, 0).Format("20060102150405")+fmt.Sprintf("%08d", itemID),
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
			return
		}
	} else {
		// 1st page
		inQuery, inArgs, err = sqlx.In("SELECT * FROM items WHERE status = ? AND category_id IN (?) ORDER BY timedateid DESC LIMIT ?",
			ItemStatusOnSale,
			categoryIDs,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
			return
		}
	}

	items := []Item{}
	err = dbx.Select(&items, inQuery, inArgs...)

	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		return
	}

	itemSimples := []ItemSimple{}
	keys := make([]string, len(items))
	for i, item := range items {
		keys[i] = strconv.Itoa(int(item.SellerID))
	}
	mGot := idToUserServer.MGet(keys)
	for _, item := range items {
		category, err := getCategoryByID(dbx, item.CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			return
		}
		var seller User
		sellerIdStr := strconv.Itoa(int(item.SellerID))
		mGot.Get(sellerIdStr, &seller)
		var simpleSeller UserSimple
		simpleSeller.ID = seller.ID
		simpleSeller.AccountName = seller.AccountName
		simpleSeller.NumSellItems = seller.NumSellItems
		itemSimples = append(itemSimples, ItemSimple{
			ID:         item.ID,
			SellerID:   item.SellerID,
			Seller:     &simpleSeller,
			Status:     item.Status,
			Name:       item.Name,
			Price:      item.Price,
			ImageURL:   getImageURL(item.ImageName),
			CategoryID: item.CategoryID,
			Category:   &category,
			CreatedAt:  item.CreatedAt.Unix(),
		})
	}

	hasNext := false
	if len(itemSimples) > ItemsPerPage {
		hasNext = true
		itemSimples = itemSimples[0:ItemsPerPage]
	}

	rni := resNewItems{
		RootCategoryID:   rootCategory.ID,
		RootCategoryName: rootCategory.CategoryName,
		Items:            itemSimples,
		HasNext:          hasNext,
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(rni)

}

func getUserItems(w http.ResponseWriter, r *http.Request) {
	userIDStr := pat.Param(r, "user_id")
	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil || userID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect user id")
		return
	}

	userSimple, err := getUserSimpleByID(dbx, userID)
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "user not found")
		return
	}

	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var itemID int64
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}

	items := []Item{}
	if itemID > 0 && createdAt > 0 {
		// paging
		err := dbx.Select(&items,
			"SELECT * FROM `items` WHERE `seller_id` = ? AND `status` IN (?,?,?) AND (`created_at` < ?  OR (`created_at` <= ? AND `id` < ?)) ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			userSimple.ID,
			ItemStatusOnSale,
			ItemStatusTrading,
			ItemStatusSoldOut,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
			return
		}
	} else {
		// 1st page
		err := dbx.Select(&items,
			"SELECT * FROM `items` WHERE `seller_id` = ? AND `status` IN (?,?,?) ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			userSimple.ID,
			ItemStatusOnSale,
			ItemStatusTrading,
			ItemStatusSoldOut,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
			return
		}
	}
	itemSimples := []ItemSimple{}
	var seller User
	var simpleSeller UserSimple
	sellerIdStr := strconv.Itoa(int(userSimple.ID))
	idToUserServer.Get(sellerIdStr, &seller)
	simpleSeller.ID = seller.ID
	simpleSeller.AccountName = seller.AccountName
	simpleSeller.NumSellItems = seller.NumSellItems
	for _, item := range items {
		category, err := getCategoryByID(dbx, item.CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			return
		}
		itemSimples = append(itemSimples, ItemSimple{
			ID:         item.ID,
			SellerID:   item.SellerID,
			Seller:     &simpleSeller,
			Status:     item.Status,
			Name:       item.Name,
			Price:      item.Price,
			ImageURL:   getImageURL(item.ImageName),
			CategoryID: item.CategoryID,
			Category:   &category,
			CreatedAt:  item.CreatedAt.Unix(),
		})
	}
	hasNext := false
	if len(itemSimples) > ItemsPerPage {
		hasNext = true
		itemSimples = itemSimples[0:ItemsPerPage]
	}
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resUserItems{
		User:    &userSimple,
		Items:   itemSimples,
		HasNext: hasNext,
	})
}

func getTransactions(w http.ResponseWriter, r *http.Request) {

	user, errCode, errMsg := getUser(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var err error
	var itemID int64
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}

	items := []Item{}
	if itemID > 0 && createdAt > 0 {
		// paging
		dbx.Select(&items,
			"SELECT * FROM `items` WHERE (`seller_id` = ? OR `buyer_id` = ?) AND (`created_at` < ?  OR (`created_at` <= ? AND `id` < ?)) ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			user.ID,
			user.ID,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			TransactionsPerPage+1,
		)
	} else {
		// 1st page
		dbx.Select(&items,
			"SELECT * FROM `items` WHERE (`seller_id` = ? OR `buyer_id` = ?) ORDER BY `created_at` DESC, `id` DESC LIMIT ?",
			user.ID,
			user.ID,
			TransactionsPerPage+1,
		)
	}
	sellerIds := make([]string, len(items))
	for i, item := range items {
		sellerIds[i] = strconv.Itoa(int(item.SellerID))
	}
	mGotIdToUser := idToUserServer.MGet(sellerIds)
	wg := sync.WaitGroup{}
	chans := make([]chan string, len(items))
	itemIdStrs := make([]string, len(items))
	for i, item := range items {
		itemIdStrs[i] = strconv.Itoa(int(item.ID))
	}
	mGotItemIdToTE := itemIdToTransactionEvidenceServer.MGet(itemIdStrs)
	itemDetails := make([]ItemDetail, 0)
	for i, item := range items {
		chans[i] = make(chan string, 1)
		var seller UserSimple
		ok := mGotIdToUser.Get(strconv.Itoa(int(item.SellerID)), &seller)
		if !ok {
			outputErrorMsg(w, http.StatusNotFound, "seller not found")
			return
		}
		category, err := getCategoryByID(dbx, item.CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			return
		}
		itemDetail := ItemDetail{
			ID:          item.ID,
			SellerID:    item.SellerID,
			Seller:      &seller,
			Status:      item.Status,
			Name:        item.Name,
			Price:       item.Price,
			Description: item.Description,
			ImageURL:    getImageURL(item.ImageName),
			CategoryID:  item.CategoryID,
			Category:    &category,
			CreatedAt:   item.CreatedAt.Unix(),
		}
		if item.BuyerID != 0 {
			buyer, err := getUserSimpleByID(dbx, item.BuyerID)
			if err != nil {
				outputErrorMsg(w, http.StatusNotFound, "buyer not found")
				return
			}
			itemDetail.BuyerID = item.BuyerID
			itemDetail.Buyer = &buyer
		}
		transactionEvidence := TransactionEvidence{}
		trExists := mGotItemIdToTE.Get(strconv.Itoa(int(item.ID)), &transactionEvidence)
		if trExists && transactionEvidence.ID > 0 {
			shipping := Shipping{}
			trIdStr := strconv.Itoa(int(transactionEvidence.ID))
			ok := transactionEvidenceToShippingsServer.Get(trIdStr, &shipping)
			if !ok {
				outputErrorMsg(w, http.StatusNotFound, "shipping not found")
				return
			}
			ssrStatus := shipping.Status
			if ssrStatus != ShippingsStatusDone {
				wg.Add(1)
				go func(i int) {
					ssr, err := APIShipmentStatus(getShipmentServiceURL(), &APIShipmentStatusReq{
						ReserveID: shipping.ReserveID,
					})
					if err != nil {
						log.Print(err)
						outputErrorMsg(w, http.StatusInternalServerError, "failed to request to shipment service")
						return
					}
					<-chans[i]
					itemDetails[i].ShippingStatus = ssr.Status
					wg.Done()
				}(i)
			}
			itemDetail.TransactionEvidenceID = transactionEvidence.ID
			itemDetail.TransactionEvidenceStatus = transactionEvidence.Status
			itemDetail.ShippingStatus = ssrStatus
		}
		itemDetails = append(itemDetails, itemDetail)
		chans[i] <- ""
		if len(itemDetails) > TransactionsPerPage {
			break
		}
	}
	wg.Wait()
	hasNext := false
	if len(itemDetails) > TransactionsPerPage {
		hasNext = true
		itemDetails = itemDetails[0:TransactionsPerPage]
	}

	rts := resTransactions{
		Items:   itemDetails,
		HasNext: hasNext,
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(rts)

}

func getItem(w http.ResponseWriter, r *http.Request) {
	itemIDStr := pat.Param(r, "item_id")
	itemID, err := strconv.ParseInt(itemIDStr, 10, 64)
	if err != nil || itemID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect item id")
		return
	}

	session := getSession(r)
	userID, ok := session.Values["user_id"]
	if !ok {
		outputErrorMsg(w, http.StatusNotFound, "no session")
		return
	}

	item := Item{}
	ok = idToItemServer.Get(strconv.Itoa(int(itemID)), &item)
	if !ok {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		return
	}
	category, err := getCategoryByID(dbx, item.CategoryID)
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "category not found")
		return
	}

	seller, err := getUserSimpleByID(dbx, item.SellerID)
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "seller not found")
		return
	}

	itemDetail := ItemDetail{
		ID:       item.ID,
		SellerID: item.SellerID,
		Seller:   &seller,
		// BuyerID
		// Buyer
		Status:      item.Status,
		Name:        item.Name,
		Price:       item.Price,
		Description: item.Description,
		ImageURL:    getImageURL(item.ImageName),
		CategoryID:  item.CategoryID,
		// TransactionEvidenceID
		// TransactionEvidenceStatus
		// ShippingStatus
		Category:  &category,
		CreatedAt: item.CreatedAt.Unix(),
	}

	if (userID == item.SellerID || userID == item.BuyerID) && item.BuyerID != 0 {
		buyer, err := getUserSimpleByID(dbx, item.BuyerID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "buyer not found")
			return
		}
		itemDetail.BuyerID = item.BuyerID
		itemDetail.Buyer = &buyer

		transactionEvidence := TransactionEvidence{}
		ok := itemIdToTransactionEvidenceServer.Get(strconv.Itoa(int(item.ID)), &transactionEvidence)
		if ok && transactionEvidence.ID > 0 {
			shipping := Shipping{}
			trIdStr := strconv.Itoa(int(transactionEvidence.ID))
			ok := transactionEvidenceToShippingsServer.Get(trIdStr, &shipping)
			if !ok {
				outputErrorMsg(w, http.StatusNotFound, "shipping not found")
				return
			}
			itemDetail.TransactionEvidenceID = transactionEvidence.ID
			itemDetail.TransactionEvidenceStatus = transactionEvidence.Status
			itemDetail.ShippingStatus = shipping.Status
		}
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(itemDetail)
}

func getQRCode(w http.ResponseWriter, r *http.Request) {
	transactionEvidenceIDStr := pat.Param(r, "transaction_evidence_id")
	transactionEvidenceID, err := strconv.ParseInt(transactionEvidenceIDStr, 10, 64)
	if err != nil || transactionEvidenceID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect transaction_evidence id")
		return
	}

	seller, errCode, errMsg := getUser(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	transactionEvidence := TransactionEvidence{}
	err = dbx.Get(&transactionEvidence, "SELECT * FROM `transaction_evidences` WHERE `id` = ?", transactionEvidenceID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		return
	}

	if transactionEvidence.SellerID != seller.ID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		return
	}

	shipping := Shipping{}
	trIdStr := strconv.Itoa(int(transactionEvidence.ID))
	ok := transactionEvidenceToShippingsServer.Get(trIdStr, &shipping)
	if !ok {
		outputErrorMsg(w, http.StatusNotFound, "shippings not found")
		return
	}
	if shipping.Status != ShippingsStatusWaitPickup && shipping.Status != ShippingsStatusShipping {
		outputErrorMsg(w, http.StatusForbidden, "qrcode not available")
		return
	}

	if len(shipping.ImgBinary) == 0 {
		outputErrorMsg(w, http.StatusInternalServerError, "empty qrcode image")
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Write(shipping.ImgBinary)
}

func getSettings(w http.ResponseWriter, r *http.Request) {
	csrfToken := getCSRFToken(r)

	user, _, errMsg := getUser(r)

	ress := resSetting{}
	ress.CSRFToken = csrfToken
	if errMsg == "" {
		ress.User = &user
	}

	ress.PaymentServiceURL = getPaymentServiceURL()

	categories := []Category{}

	err := dbx.Select(&categories, "SELECT * FROM `categories`")
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		return
	}
	ress.Categories = categories

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(ress)
}

func getReports(w http.ResponseWriter, r *http.Request) {
	transactionEvidences := make([]TransactionEvidence, 0)
	err := dbx.Select(&transactionEvidences, "SELECT * FROM `transaction_evidences` WHERE `id` > 15007")
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error"+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(transactionEvidences)
}
