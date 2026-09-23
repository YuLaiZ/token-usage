package querier

// dayhour.go 提供日期×小时热力矩阵:仪表板 Web 端「范围内每一天一行、
// 每行 24 个小时格」的统一数据源。单日范围即一行;2~7 天逐日逐小时;
// 8~31 天的 4 小时时段与 32 天以上的逐日日历均由消费方按同一份逐小时
// 数据确定性合并,后端不再为各形态分别聚合。
//
// 与 HeatmapMatrix(星期×小时)同机制:双维 AggregateDimensionView 的
// rawKeys 交点填格,同一读事务内与 totals/dimensions/sessions 快照一致。

import "context"

// DayHourMatrix 是日期×小时矩阵:Days 为升序日期(与请求 dates 一致,
// 含无数据的零值日);Hours 为小时桶键 "00".."23";Totals[di] 为该日
// 全天合计(无数据日为 0);Values[di][hi] 为该日该小时的 total。
type DayHourMatrix struct {
	Days   []string
	Hours  []string
	Totals []int64
	Values [][]int64
}

// DayHourMatrix 计算日期×小时交点矩阵(小时按本机时区归属,与 date 列
// 同一时区语义)。dates 必须为升序 YYYY-MM-DD 列表;输出与输入逐日对齐,
// 无数据日为全零行。
func (q *Querier) DayHourMatrix(ctx context.Context, dates []string) (*DayHourMatrix, error) {
	if _, err := q.readyContext(ctx); err != nil {
		return nil, err
	}
	rows, _, err := q.AggregateDimensionView(ctx, dates, DimensionView{
		Dimensions: []string{"day", "hour"},
		TitleEn:    "heatmap", TitleZh: "heatmap",
	})
	if err != nil {
		return nil, err
	}
	type cellKey struct{ day, hour string }
	cell := make(map[cellKey]int64, len(rows))
	for _, row := range rows {
		if len(row.rawKeys) != 2 {
			continue
		}
		cell[cellKey{row.rawKeys[0], row.rawKeys[1]}] += row.Agg.TotalTokens
	}

	m := &DayHourMatrix{
		Days:   append([]string(nil), dates...),
		Hours:  append([]string(nil), hourBucketTable[:]...),
		Totals: make([]int64, len(dates)),
		Values: make([][]int64, len(dates)),
	}
	for di, day := range dates {
		m.Values[di] = make([]int64, len(hourBucketTable))
		for hi, hour := range m.Hours {
			v := cell[cellKey{day, hour}]
			m.Values[di][hi] = v
			m.Totals[di] += v
		}
	}
	return m, nil
}
