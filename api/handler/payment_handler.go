package handler

// * +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
// * Copyright 2023 The Geek-AI Authors. All rights reserved.
// * Use of this source code is governed by a Apache-2.0 license
// * that can be found in the LICENSE file.
// * @Author yangjian102621@163.com
// * +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++

import (
	"embed"
	"encoding/base64"
	"fmt"
	"geekai/core"
	"geekai/core/types"
	"geekai/service"
	"geekai/service/payment"
	"geekai/store/model"
	"geekai/utils"
	"geekai/utils/resp"
	"github.com/shopspring/decimal"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type PayWay struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// PaymentHandler 支付服务回调 handler
type PaymentHandler struct {
	BaseHandler
	alipayService    *payment.AlipayService
	huPiPayService   *payment.HuPiPayService
	geekPayService   *payment.GeekPayService
	wechatPayService *payment.WechatPayService
	snowflake        *service.Snowflake
	fs               embed.FS
	lock             sync.Mutex
	signKey          string // 用来签名的随机秘钥
}

func NewPaymentHandler(
	server *core.AppServer,
	alipayService *payment.AlipayService,
	huPiPayService *payment.HuPiPayService,
	geekPayService *payment.GeekPayService,
	wechatPayService *payment.WechatPayService,
	db *gorm.DB,
	snowflake *service.Snowflake,
	fs embed.FS) *PaymentHandler {
	return &PaymentHandler{
		alipayService:    alipayService,
		huPiPayService:   huPiPayService,
		geekPayService:   geekPayService,
		wechatPayService: wechatPayService,
		snowflake:        snowflake,
		fs:               fs,
		lock:             sync.Mutex{},
		BaseHandler: BaseHandler{
			App: server,
			DB:  db,
		},
		signKey: utils.RandString(32),
	}
}

func (h *PaymentHandler) DoPay(c *gin.Context) {
	orderNo := h.GetTrim(c, "order_no")
	t := h.GetInt(c, "t", 0)
	sign := h.GetTrim(c, "sign")
	signStr := fmt.Sprintf("%s-%d-%s", orderNo, t, h.signKey)
	newSign := utils.Sha256(signStr)
	if newSign != sign {
		resp.ERROR(c, "订单签名错误！")
		return
	}

	// 检查二维码是否过期
	if time.Now().Unix()-int64(t) > int64(h.App.SysConfig.OrderPayTimeout) {
		resp.ERROR(c, "支付二维码已过期，请重新生成！")
		return
	}

	if orderNo == "" {
		resp.ERROR(c, types.InvalidArgs)
		return
	}

	var order model.Order
	res := h.DB.Where("order_no = ?", orderNo).First(&order)
	if res.Error != nil {
		resp.ERROR(c, "Order not found")
		return
	}

	// fix: 这里先检查一下订单状态，如果已经支付了，就直接返回
	if order.Status == types.OrderPaidSuccess {
		resp.ERROR(c, "订单已支付成功，无需重复支付！")
		return
	}

	// 更新扫码状态
	h.DB.Model(&order).UpdateColumn("status", types.OrderScanned)

	if order.PayWay == "alipay" { // 支付宝
		amount := fmt.Sprintf("%.2f", order.Amount)
		uri, err := h.alipayService.PayUrlMobile(order.OrderNo, amount, order.Subject)
		if err != nil {
			resp.ERROR(c, "error with generate pay url: "+err.Error())
			return
		}

		c.Redirect(302, uri)
		return
	} else if order.PayWay == "hupi" { // 虎皮椒支付
		params := payment.HuPiPayReq{
			Version:      "1.1",
			TradeOrderId: orderNo,
			TotalFee:     fmt.Sprintf("%f", order.Amount),
			Title:        order.Subject,
			NotifyURL:    h.App.Config.HuPiPayConfig.NotifyURL,
			WapName:      "极客学长",
		}
		r, err := h.huPiPayService.Pay(params)
		if err != nil {
			resp.ERROR(c, err.Error())
			return
		}

		c.Redirect(302, r.URL)
	} else if order.PayWay == "wechat" {
		uri, err := h.wechatPayService.PayUrlNative(order.OrderNo, int(order.Amount*100), order.Subject)
		if err != nil {
			resp.ERROR(c, err.Error())
			return
		}

		c.Redirect(302, uri)
	} else if order.PayWay == "geek" {
		params := payment.GeekPayParams{
			OutTradeNo: orderNo,
			Method:     "web",
			Name:       order.Subject,
			Money:      fmt.Sprintf("%f", order.Amount),
			ClientIP:   c.ClientIP(),
			Device:     "pc",
			Type:       "alipay",
		}

		s, err := h.geekPayService.Pay(params)
		if err != nil {
			resp.ERROR(c, err.Error())
			return
		}
		resp.SUCCESS(c, s)
	}
	//resp.ERROR(c, "Invalid operations")
}

// PayQrcode 生成支付 URL 二维码
func (h *PaymentHandler) PayQrcode(c *gin.Context) {
	var data struct {
		PayWay    string `json:"pay_way"`    // 支付方式
		PayType   string `json:"pay_type"`   // 支付类别：wechat,alipay,qq...
		ProductId uint   `json:"product_id"` // 支付产品ID
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		resp.ERROR(c, types.InvalidArgs)
		return
	}

	var product model.Product
	res := h.DB.First(&product, data.ProductId)
	if res.Error != nil {
		resp.ERROR(c, "Product not found")
		return
	}

	orderNo, err := h.snowflake.Next(false)
	if err != nil {
		resp.ERROR(c, "error with generate trade no: "+err.Error())
		return
	}
	user, err := h.GetLoginUser(c)
	if err != nil {
		resp.NotAuth(c)
		return
	}

	var notifyURL string
	switch data.PayWay {
	case "hupi":
		notifyURL = h.App.Config.HuPiPayConfig.NotifyURL
		break
	case "geek":
		notifyURL = h.App.Config.GeekPayConfig.NotifyURL
		break
	case "alipay": // 支付宝商户支付
		notifyURL = h.App.Config.AlipayConfig.NotifyURL
		break
	case "wechat": // 微信商户支付
		notifyURL = h.App.Config.WechatPayConfig.NotifyURL
	default:
		resp.ERROR(c, "Invalid pay way")
		return

	}
	// 创建订单
	remark := types.OrderRemark{
		Days:     product.Days,
		Power:    product.Power,
		Name:     product.Name,
		Price:    product.Price,
		Discount: product.Discount,
	}

	amount, _ := decimal.NewFromFloat(product.Price).Sub(decimal.NewFromFloat(product.Discount)).Float64()
	order := model.Order{
		UserId:    user.Id,
		Username:  user.Username,
		ProductId: product.Id,
		OrderNo:   orderNo,
		Subject:   product.Name,
		Amount:    amount,
		Status:    types.OrderNotPaid,
		PayWay:    data.PayWay,
		PayType:   data.PayType,
		Remark:    utils.JsonEncode(remark),
	}
	res = h.DB.Create(&order)
	if res.Error != nil || res.RowsAffected == 0 {
		resp.ERROR(c, "error with create order: "+res.Error.Error())
		return
	}

	var logo string
	switch data.PayType {
	case "alipay":
		logo = "res/img/alipay.jpg"
		break
	case "wechat":
		logo = "res/img/wechat-pay.jpg"
		break
	case "qq":
		logo = "res/img/qq-pay.jpg"
		break
	default:
		logo = "res/img/geek-pay.jpg"

	}
	if data.PayType == "alipay" {
		logo = "res/img/alipay.jpg"
	} else if data.PayType == "wechat" {
		logo = "res/img/wechat-pay.jpg"
	}
	file, err := h.fs.Open(logo)
	if err != nil {
		resp.ERROR(c, "error with open qrcode log file: "+err.Error())
		return
	}

	parse, err := url.Parse(notifyURL)
	if err != nil {
		resp.ERROR(c, err.Error())
		return
	}
	timestamp := time.Now().Unix()
	signStr := fmt.Sprintf("%s-%s-%d-%s", orderNo, data.PayWay, timestamp, h.signKey)
	sign := utils.Sha256(signStr)
	payUrl := fmt.Sprintf("%s://%s/api/payment/doPay?order_no=%s&pay_way=%s&pay_type=%s&t=%d&sign=%s", parse.Scheme, parse.Host, orderNo, data.PayWay, data.PayType, timestamp, sign)
	imgData, err := utils.GenQrcode(payUrl, 400, file)
	if err != nil {
		resp.ERROR(c, err.Error())
		return
	}
	imgDataBase64 := base64.StdEncoding.EncodeToString(imgData)
	resp.SUCCESS(c, gin.H{"order_no": orderNo, "image": fmt.Sprintf("data:image/jpg;base64, %s", imgDataBase64), "url": payUrl})
}

// Mobile 移动端支付
func (h *PaymentHandler) Mobile(c *gin.Context) {
	var data struct {
		PayWay    string `json:"pay_way"`  // 支付方式
		PayType   string `json:"pay_type"` // 支付类别：wechat,alipay,qq...
		ProductId uint   `json:"product_id"`
	}
	if err := c.ShouldBindJSON(&data); err != nil {
		resp.ERROR(c, types.InvalidArgs)
		return
	}

	var product model.Product
	res := h.DB.First(&product, data.ProductId)
	if res.Error != nil {
		resp.ERROR(c, "Product not found")
		return
	}

	orderNo, err := h.snowflake.Next(false)
	if err != nil {
		resp.ERROR(c, "error with generate trade no: "+err.Error())
		return
	}
	user, err := h.GetLoginUser(c)
	if err != nil {
		resp.NotAuth(c)
		return
	}

	amount, _ := decimal.NewFromFloat(product.Price).Sub(decimal.NewFromFloat(product.Discount)).Float64()
	var notifyURL, returnURL string
	var payURL string
	switch data.PayWay {
	case "hupi":
		notifyURL = h.App.Config.HuPiPayConfig.NotifyURL
		returnURL = h.App.Config.HuPiPayConfig.ReturnURL
		parse, _ := url.Parse(h.App.Config.HuPiPayConfig.ReturnURL)
		baseURL := fmt.Sprintf("%s://%s", parse.Scheme, parse.Host)
		params := payment.HuPiPayReq{
			Version:      "1.1",
			TradeOrderId: orderNo,
			TotalFee:     fmt.Sprintf("%f", amount),
			Title:        product.Name,
			NotifyURL:    notifyURL,
			ReturnURL:    returnURL,
			CallbackURL:  returnURL,
			WapName:      "极客学长",
			WapUrl:       baseURL,
			Type:         "WAP",
		}
		r, err := h.huPiPayService.Pay(params)
		if err != nil {
			errMsg := "error with generating Pay Hupi URL: " + err.Error()
			logger.Error(errMsg)
			resp.ERROR(c, errMsg)
			return
		}
		payURL = r.URL
	case "geek":
		//totalFee := decimal.NewFromFloat(product.Price).Sub(decimal.NewFromFloat(product.Discount)).Mul(decimal.NewFromInt(100)).IntPart()
		//params := url.Values{}
		//params.Add("total_fee", fmt.Sprintf("%d", totalFee))
		//params.Add("out_trade_no", orderNo)
		//params.Add("body", product.Name)
		//params.Add("notify_url", notifyURL)
		//params.Add("auto", "0")
		//payURL = h.geekPayService.Pay(params)
	case "alipay":
		payURL, err = h.alipayService.PayUrlMobile(orderNo, fmt.Sprintf("%.2f", amount), product.Name)
		if err != nil {
			errMsg := "error with generating Alipay URL: " + err.Error()
			resp.ERROR(c, errMsg)
			return
		}
	case "wechat":
		payURL, err = h.wechatPayService.PayUrlH5(orderNo, int(amount*100), product.Name, c.ClientIP())
		if err != nil {
			errMsg := "error with generating Wechat URL: " + err.Error()
			logger.Error(errMsg)
			resp.ERROR(c, errMsg)
			return
		}
	default:
		resp.ERROR(c, "Unsupported pay way: "+data.PayWay)
		return
	}
	// 创建订单
	remark := types.OrderRemark{
		Days:     product.Days,
		Power:    product.Power,
		Name:     product.Name,
		Price:    product.Price,
		Discount: product.Discount,
	}

	order := model.Order{
		UserId:    user.Id,
		Username:  user.Username,
		ProductId: product.Id,
		OrderNo:   orderNo,
		Subject:   product.Name,
		Amount:    amount,
		Status:    types.OrderNotPaid,
		PayWay:    data.PayWay,
		PayType:   data.PayType,
		Remark:    utils.JsonEncode(remark),
	}
	res = h.DB.Create(&order)
	if res.Error != nil || res.RowsAffected == 0 {
		resp.ERROR(c, "error with create order: "+res.Error.Error())
		return
	}

	resp.SUCCESS(c, gin.H{"url": payURL, "order_no": orderNo})
}

// 异步通知回调公共逻辑
func (h *PaymentHandler) notify(orderNo string, tradeNo string) error {
	var order model.Order
	res := h.DB.Where("order_no = ?", orderNo).First(&order)
	if res.Error != nil {
		err := fmt.Errorf("error with fetch order: %v", res.Error)
		logger.Error(err)
		return err
	}

	h.lock.Lock()
	defer h.lock.Unlock()

	// 已支付订单，直接返回
	if order.Status == types.OrderPaidSuccess {
		return nil
	}

	var user model.User
	res = h.DB.First(&user, order.UserId)
	if res.Error != nil {
		err := fmt.Errorf("error with fetch user info: %v", res.Error)
		logger.Error(err)
		return err
	}

	var remark types.OrderRemark
	err := utils.JsonDecode(order.Remark, &remark)
	if err != nil {
		err := fmt.Errorf("error with decode order remark: %v", err)
		logger.Error(err)
		return err
	}

	var opt string
	var power int
	if remark.Days > 0 { // VIP 充值
		if user.ExpiredTime >= time.Now().Unix() {
			user.ExpiredTime = time.Unix(user.ExpiredTime, 0).AddDate(0, 0, remark.Days).Unix()
			opt = "VIP充值，VIP 没到期，只延期不增加算力"
		} else {
			user.ExpiredTime = time.Now().AddDate(0, 0, remark.Days).Unix()
			user.Power += h.App.SysConfig.VipMonthPower
			power = h.App.SysConfig.VipMonthPower
			opt = "VIP充值"
		}
		user.Vip = true
	} else { // 充值点卡，直接增加次数即可
		user.Power += remark.Power
		opt = "点卡充值"
		power = remark.Power
	}

	// 更新用户信息
	res = h.DB.Updates(&user)
	if res.Error != nil {
		err := fmt.Errorf("error with update user info: %v", res.Error)
		logger.Error(err)
		return err
	}

	// 更新订单状态
	order.PayTime = time.Now().Unix()
	order.Status = types.OrderPaidSuccess
	order.TradeNo = tradeNo
	res = h.DB.Updates(&order)
	if res.Error != nil {
		err := fmt.Errorf("error with update order info: %v", res.Error)
		logger.Error(err)
		return err
	}

	// 更新产品销量
	h.DB.Model(&model.Product{}).Where("id = ?", order.ProductId).UpdateColumn("sales", gorm.Expr("sales + ?", 1))

	// 记录算力充值日志
	if power > 0 {
		h.DB.Create(&model.PowerLog{
			UserId:    user.Id,
			Username:  user.Username,
			Type:      types.PowerRecharge,
			Amount:    power,
			Balance:   user.Power,
			Mark:      types.PowerAdd,
			Model:     order.PayWay,
			Remark:    fmt.Sprintf("%s，金额：%f，订单号：%s", opt, order.Amount, order.OrderNo),
			CreatedAt: time.Now(),
		})
	}

	return nil
}

// GetPayWays 获取支付方式
func (h *PaymentHandler) GetPayWays(c *gin.Context) {
	payWays := make([]gin.H, 0)
	if h.App.Config.AlipayConfig.Enabled {
		payWays = append(payWays, gin.H{"pay_way": "alipay", "pay_type": "alipay"})
	}
	if h.App.Config.HuPiPayConfig.Enabled {
		payWays = append(payWays, gin.H{"pay_way": "hupi", "pay_type": h.App.Config.HuPiPayConfig.Name})
	}
	if h.App.Config.GeekPayConfig.Enabled {
		payWays = append(payWays, gin.H{"pay_way": "geek", "pay_type": "alipay"})
		payWays = append(payWays, gin.H{"pay_way": "geek", "pay_type": "wechat"})
		payWays = append(payWays, gin.H{"pay_way": "geek", "pay_type": "qq"})
		payWays = append(payWays, gin.H{"pay_way": "geek", "pay_type": "jd"})
		payWays = append(payWays, gin.H{"pay_way": "geek", "pay_type": "douyin"})
		payWays = append(payWays, gin.H{"pay_way": "geek", "pay_type": "paypal"})
	}
	if h.App.Config.WechatPayConfig.Enabled {
		payWays = append(payWays, gin.H{"pay_way": "wechat", "pay_type": "wechat"})
	}
	resp.SUCCESS(c, payWays)
}

// HuPiPayNotify 虎皮椒支付异步回调
func (h *PaymentHandler) HuPiPayNotify(c *gin.Context) {
	err := c.Request.ParseForm()
	if err != nil {
		c.String(http.StatusOK, "fail")
		return
	}

	orderNo := c.Request.Form.Get("trade_order_id")
	tradeNo := c.Request.Form.Get("open_order_id")
	logger.Infof("收到虎皮椒订单支付回调，订单 NO：%s，交易流水号：%s", orderNo, tradeNo)

	if err = h.huPiPayService.Check(tradeNo); err != nil {
		logger.Error("订单校验失败：", err)
		c.String(http.StatusOK, "fail")
		return
	}
	err = h.notify(orderNo, tradeNo)
	if err != nil {
		c.String(http.StatusOK, "fail")
		return
	}

	c.String(http.StatusOK, "success")
}

// AlipayNotify 支付宝支付回调
func (h *PaymentHandler) AlipayNotify(c *gin.Context) {
	err := c.Request.ParseForm()
	if err != nil {
		c.String(http.StatusOK, "fail")
		return
	}

	// TODO：验证交易签名
	res := h.alipayService.TradeVerify(c.Request)
	logger.Infof("验证支付结果：%+v", res)
	if !res.Success() {
		logger.Error("订单校验失败：", res.Message)
		c.String(http.StatusOK, "fail")
		return
	}

	tradeNo := c.Request.Form.Get("trade_no")
	err = h.notify(res.OutTradeNo, tradeNo)
	if err != nil {
		c.String(http.StatusOK, "fail")
		return
	}

	c.String(http.StatusOK, "success")
}

// PayJsNotify PayJs 支付异步回调
func (h *PaymentHandler) PayJsNotify(c *gin.Context) {
	err := c.Request.ParseForm()
	if err != nil {
		c.String(http.StatusOK, "fail")
		return
	}

	orderNo := c.Request.Form.Get("out_trade_no")
	returnCode := c.Request.Form.Get("return_code")
	logger.Infof("收到PayJs订单支付回调，订单 NO：%s，支付结果代码：%v", orderNo, returnCode)
	// 支付失败
	if returnCode != "1" {
		return
	}

	// 校验订单支付状态
	tradeNo := c.Request.Form.Get("payjs_order_id")
	//err = h.geekPayService.TradeVerify(tradeNo)
	//if err != nil {
	//	logger.Error("订单校验失败：", err)
	//	c.String(http.StatusOK, "fail")
	//	return
	//}

	err = h.notify(orderNo, tradeNo)
	if err != nil {
		c.String(http.StatusOK, "fail")
		return
	}

	c.String(http.StatusOK, "success")
}

// WechatPayNotify 微信商户支付异步回调
func (h *PaymentHandler) WechatPayNotify(c *gin.Context) {
	err := c.Request.ParseForm()
	if err != nil {
		c.String(http.StatusOK, "fail")
		return
	}

	result := h.wechatPayService.TradeVerify(c.Request)
	if !result.Success() {
		logger.Error("订单校验失败：", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "FAIL",
			"message": err.Error(),
		})
		return
	}

	err = h.notify(result.OutTradeNo, result.TradeId)
	if err != nil {
		c.String(http.StatusOK, "fail")
		return
	}

	c.String(http.StatusOK, "success")
}
