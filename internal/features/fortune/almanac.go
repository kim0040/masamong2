package fortune

import (
	"container/list"
	"slices"

	"github.com/6tail/lunar-go/LunarUtil"
)

// The day's 宜 (good for) and 忌 (avoid) lists come from lunar-go's almanac table (MIT), which is keyed
// by the month and day 간지. Its items are classic almanac activities (嫁娶, 动土, 安葬 …); a reader
// gets them as a few everyday groups instead, named in the catalogs (fortune.saju.act.<id>). Funeral
// and burial rites, medical treatment and lawsuits are left out on purpose: a fortune does not tell
// anyone to see or skip a doctor, or to go to court.

// activityOf maps lunar-go's item names to a group id; an item not listed here is not shown.
var activityOf = map[string]string{}

// activityGroups are the groups in the order a reading lists them, with their lunar-go items.
var activityGroups = []struct {
	id    string
	items []string
}{
	{"wedding", []string{"嫁娶", "订婚", "纳采", "问名", "纳婿", "订盟", "合帐"}},
	{"moving", []string{"入宅", "移徙", "安床", "分居"}},
	{"travel", []string{"出行", "乘船", "渡水", "归宁"}},
	{"trade", []string{"开市", "挂匾", "立券交易", "交易", "立券", "开仓", "出货财"}},
	{"money", []string{"纳财", "求财", "置产", "买车"}},
	{"meeting", []string{"会亲友", "会友", "见贵"}},
	{"new_role", []string{"赴任", "雇佣", "进人口"}},
	{"study", []string{"习艺", "入学"}},
	{"grooming", []string{"沐浴", "理发", "剃头", "整手足甲", "裁衣", "挽面", "开容", "冠笄"}},
	{"cleaning", []string{"扫舍", "解除", "塞穴", "断蚁"}},
	{"repair", []string{"修造", "起基", "动土", "上梁", "竖柱", "开井开池", "作陂放水", "拆卸", "破屋", "坏垣", "补垣",
		"伐木做梁", "作灶", "开柱眼", "穿屏扇架", "盖屋合脊", "开厕", "造仓", "平治道涂", "造桥", "作厕", "筑堤", "开池",
		"伐木", "开渠", "掘井", "放水", "造屋", "合脊", "造畜稠", "修门", "定磉", "作梁", "修饰垣墙", "架马", "起基动土",
		"破屋坏垣", "盖屋", "造仓库", "安门", "安机械", "造车器", "安碓磑", "安机", "鼓铸", "造船"}},
	{"craft", []string{"雕刻", "塑绘", "经络", "酝酿", "作染"}},
	{"garden", []string{"栽种", "牧养", "纳畜", "取渔", "结网", "割蜜", "教牛马"}},
	{"prayer", []string{"祭祀", "祈福", "求嗣", "开光", "齐醮", "斋醮", "酬神", "造庙", "祀灶", "焚香", "谢土", "出火", "安香", "普渡"}},
}

// activityDropped are lunar-go items deliberately not shown (see above), and its "nothing" markers.
var activityDropped = map[string]bool{
	"修坟": true, "启钻": true, "破土": true, "安葬": true, "立碑": true, "成服": true, "除服": true, "开生坟": true,
	"合寿木": true, "入殓": true, "移柩": true, "行丧": true, "归岫": true,
	"探病": true, "针灸": true, "求医": true, "治病": true, "求医疗病": true, "词讼": true, "捕捉": true, "畋猎": true,
	"诸事不宜": true, "馀事勿取": true, "无": true,
}

// ActivityIDs are the group ids, in reading order.
var ActivityIDs []string

func init() {
	for _, g := range activityGroups {
		ActivityIDs = append(ActivityIDs, g.id)
		for _, it := range g.items {
			activityOf[it] = g.id
		}
	}
}

// ActivityLimit is how many groups a day lists on each side.
const ActivityLimit = 4

// Activities are the day's good and avoid groups (at most ActivityLimit each, in lunar-go's order). A
// group that lunar-go puts on both sides that day (修造 good but 动土 to avoid) is left off both.
func Activities(month, day Pillar) (yi, ji []string) {
	yiAll := groups(LunarUtil.GetDayYi(hanzi(month), hanzi(day)))
	jiAll := groups(LunarUtil.GetDayJi(hanzi(month), hanzi(day)))
	in := func(gs []string, g string) bool { return slices.Contains(gs, g) }
	for _, g := range yiAll {
		if !in(jiAll, g) && len(yi) < ActivityLimit {
			yi = append(yi, g)
		}
	}
	for _, g := range jiAll {
		if !in(yiAll, g) && len(ji) < ActivityLimit {
			ji = append(ji, g)
		}
	}
	return yi, ji
}

// groups are the distinct groups of lunar-go's items, in their order.
func groups(items *list.List) []string {
	var out []string
	for e := items.Front(); e != nil; e = e.Next() {
		name, _ := e.Value.(string)
		if g, ok := activityOf[name]; ok && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

// hanzi is a pillar as lunar-go writes it ("甲子").
func hanzi(p Pillar) string { return LunarUtil.GAN[p.Stem+1] + LunarUtil.ZHI[p.Branch+1] }
