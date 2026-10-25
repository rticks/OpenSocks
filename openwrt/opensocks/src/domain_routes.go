package main

// Shared routing data and helpers. The former Clash/mihomo configuration
// generator lived here; the in-process engine only needs the domain list, the
// protocol ordering helper and a couple of small utilities.

// cnDomainSuffixes covers the major Chinese streaming/media/social services so
// Smart mode proxies them without waiting for a DNS-derived address. The
// server's clash_rule endpoint was removed (404 on current servers), so this
// built-in list replaces it.
var cnDomainSuffixes = []string{
	// video / streaming
	"bilibili.com", "b23.tv", "bilivideo.com", "bilivideo.cn", "bilibili.cn", "hdslb.com",
	"iqiyi.com", "iqiyipic.com", "qiyi.com", "71.am",
	"youku.com", "youkucdn.com", "youku.net", "tudou.com",
	"qq.com", "gtimg.com", "qpic.cn", "qqvideo.com", "tencent.com", "tencentmusic.com",
	"y.qq.com", "music.qq.com", "qqmusic.com",
	"mgtv.com", "hunantv.com", "imgsfs.com",
	"sohu.com", "sohucs.com",
	"le.com", "letv.com", "lemall.com",
	"163.com", "netease.com", "music.163.com", "126.net",
	"kugou.com", "kuwo.cn", "kuaishou.com", "douyin.com", "douyincdn.com",
	"kuaishoucdn.com", "zhibo.tv",
	"cntv.cn", "cctv.com", "wasu.cn", "wasuband.cn",
	"pptv.com", "china.com", "fun.tv",
	// social / messaging
	"weibo.com", "weibo.cn", "sina.com.cn", "sinaimg.cn", "sinajs.cn",
	"weixin.qq.com", "wechat.com", "wechatinc.com", "wx.qq.com",
	"zhihu.com", "zhimg.com",
	"douban.com", "doubanio.com",
	"xiaohongshu.com", "xhscdn.com",
	"tieba.baidu.com", "baidu.com", "bdstatic.com", "bdimg.com", "bcebos.com", "baidupcs.com",
	// e-commerce
	"taobao.com", "tmall.com", "alicdn.com", "alibaba.com", "alibabacloud.com", "aliyuncs.com",
	"jd.com", "jdcdn.com", "360buyimg.com", "pinduoduo.com", "yangkeduo.com",
	"meituan.com", "dianping.com", "ele.me", "kaola.com", "suning.com",
	// payment / travel
	"alipay.com", "wechatpay.com", "unionpay.com", "12306.cn", "ctrip.com", "qunar.com",
	// maps / news
	"amap.com", "autonavi.com",
	"gmw.cn", "people.com.cn", "xinhuanet.com", "chinanews.com", "thepaper.cn", "ifeng.com",
	// finance
	"eastmoney.com", "sinafinance.cn", "hexun.com", "xueqiu.com", "cs.com.cn", "10jqka.com.cn",
	// cloud / common CN CDN
	"myqcloud.com", "qcloud.com", "chinacache.com", "cdngslb.com",
	// domestic games / launchers / account and payment services
	"mihoyo.com", "hoyolab.com", "yuanshen.com", "bh3.com", "benghuai.com", "mhyurl.cn",
	"game.qq.com", "ieg.com", "wegame.com.cn", "dnf.qq.com", "lol.qq.com", "pvp.qq.com",
	// 王者荣耀: official site, game assets, login/SDK, telemetry and push.
	// Match-by-suffix includes every subdomain. PVP server IPs are additionally
	// covered by the China CIDR set, including UDP endpoints without DNS names.
	"game.gtimg.cn", "dlied5.qq.com", "ossweb-img.qq.com", "sqimg.qq.com",
	"msdk.qq.com", "gcloud.qq.com", "tpns.tencent.com", "bugly.qq.com",
	"neteasegames.com", "163yun.com", "xyq.163.com", "mc.163.com", "my.163.com",
	"wanmei.com", "perfectworld.com.cn", "changyou.com", "cy.com", "shengqugames.com", "sdo.com",
	"biligame.com", "biligame.net", "4399.com", "7k7k.com", "9game.cn", "uc.cn",
	"kuaikanmanhua.com", "acfun.cn", "acfun.com", "douyu.com", "douyucdn.cn", "huya.com",
	// payment, identity, banking and mini-program infrastructure
	"alipayobjects.com", "alipaydev.com", "antfin.com", "蚂蚁.com", "tenpay.com", "95516.com",
	"weixinbridge.com", "qlogo.cn", "qcloudimg.com", "qqmail.com", "foxmail.com",
	"cmbchina.com", "icbc.com.cn", "ccb.com", "abchina.com", "bankcomm.com", "boc.cn",
}

func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// orderedBoots orders the protocol variants so the line's declared protocol
// (Config.proto) is tried first: 0=SS, 1=GTS, 3=SSW, 5=Trojan.
func orderedBoots(c *connectConfig) []*bootsInfo {
	if c == nil {
		return nil
	}
	first := map[int]*bootsInfo{0: c.SSConf, 1: c.GTSConf, 3: c.SSWConf, 5: c.TrojanConf}[c.Proto]
	rest := []*bootsInfo{c.SSConf, c.SSWConf, c.GTSConf, c.TrojanConf}
	out := make([]*bootsInfo, 0, len(rest))
	if first != nil {
		out = append(out, first)
	}
	for _, b := range rest {
		if b != nil && b != first {
			out = append(out, b)
		}
	}
	return out
}
