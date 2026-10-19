import * as echarts from "echarts/core";
import { LineChart } from "echarts/charts";
import { GridComponent, LegendComponent, TooltipComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import type { EChartsCoreOption } from "echarts/core";
import ReactEChartsCoreModule from "echarts-for-react/lib/core";

// 该包只提供 CJS 入口，Vite 的默认导入会拿到模块对象而不是组件本身。
const ReactEChartsCore =
  (
    ReactEChartsCoreModule as unknown as {
      default?: typeof ReactEChartsCoreModule;
    }
  ).default ?? ReactEChartsCoreModule;

// 按需注册：未注册的图表类型或组件会让 option 静默失效，新增图表时同步扩展这里。
echarts.use([
  LineChart,
  GridComponent,
  TooltipComponent,
  LegendComponent,
  CanvasRenderer,
]);

export function Chart(props: {
  option: EChartsCoreOption;
  height?: number;
}) {
  return (
    <ReactEChartsCore
      echarts={echarts}
      option={props.option}
      notMerge
      style={{ height: props.height ?? 260, width: "100%" }}
      opts={{ renderer: "canvas" }}
    />
  );
}
