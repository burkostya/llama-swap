<script lang="ts">
  import { onMount } from "svelte";
  import {
    Chart,
    ScatterController,
    PointElement,
    LinearScale,
    LogarithmicScale,
    Tooltip,
    type ChartOptions,
  } from "chart.js";
  import { isDarkMode } from "../stores/theme";
  import { formatPromptTick, promptLogBounds, promptTooltipLines, type PromptChartPoint } from "../lib/promptScatter";

  Chart.register(ScatterController, PointElement, LinearScale, LogarithmicScale, Tooltip);

  let { points }: { points: PromptChartPoint[] } = $props();
  let canvas: HTMLCanvasElement;
  let chart: Chart<"scatter", PromptChartPoint[]>;
  const nf = new Intl.NumberFormat();

  function buildOptions(dark: boolean, data: PromptChartPoint[]): ChartOptions<"scatter"> {
    const tick = dark ? "#9ca3af" : "#6b7280";
    const grid = dark ? "rgba(255,255,255,0.08)" : "rgba(0,0,0,0.08)";
    const tooltipText = dark ? "#f3f4f6" : "#111827";
    const bounds = promptLogBounds(data);
    return {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      interaction: { mode: "nearest", intersect: false },
      plugins: {
        legend: { display: false },
        tooltip: {
          backgroundColor: dark ? "#1f2937" : "#ffffff",
          titleColor: tooltipText,
          bodyColor: tooltipText,
          borderColor: dark ? "#374151" : "#e5e7eb",
          borderWidth: 1,
          displayColors: false,
          // Overlapping requests can tie for nearest; show one instead of an
          // unbounded tooltip with a line for every coincident point.
          filter: (_item, index) => index === 0,
          callbacks: {
            title: (items) => (items[0]?.raw as PromptChartPoint | undefined)?.model ?? "",
            label: (item) => promptTooltipLines(item.raw as PromptChartPoint),
          },
        },
      },
      scales: {
        x: {
          type: "logarithmic",
          min: bounds.min,
          max: bounds.max,
          ticks: { color: tick, maxRotation: 0, font: { size: 10 }, callback: (value) => formatPromptTick(Number(value)) },
          grid: { color: grid },
          title: { display: true, text: "Prompt tokens (log)", color: tick, font: { size: 10 } },
        },
        y: {
          type: "linear",
          beginAtZero: true,
          grace: "5%",
          ticks: { color: tick, font: { size: 10 }, maxTicksLimit: 5 },
          grid: { color: grid },
          title: { display: true, text: "Prefill (t/s)", color: tick, font: { size: 10 } },
        },
      },
    };
  }

  onMount(() => {
    chart = new Chart(canvas, {
      type: "scatter",
      data: {
        datasets: [{
          data: [...points],
          backgroundColor: "rgba(245, 158, 11, 0.6)",
          pointRadius: 3,
          pointHoverRadius: 5,
          pointHitRadius: 5,
          clip: false,
        }],
      },
      options: buildOptions($isDarkMode, points),
    });
    return () => chart.destroy();
  });

  $effect(() => {
    if (!chart) return;
    const data = points;
    const dark = $isDarkMode;
    chart.data.datasets[0].data = [...data];
    chart.options = buildOptions(dark, data);
    chart.update("none");
  });
</script>

<div class="mt-2 h-[180px] w-full">
  <canvas
    bind:this={canvas}
    aria-label="Prefill speed by prompt token count, {nf.format(points.length)} requests plotted"
  >Prefill speed by prompt token count, {nf.format(points.length)} requests plotted.</canvas>
</div>
<div class="text-muted-foreground mt-1 text-xs">{nf.format(points.length)} requests plotted</div>
