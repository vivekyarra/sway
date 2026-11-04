"use client";

import { AllCommunityModule, ModuleRegistry, themeQuartz, type ColDef, type GridOptions } from "ag-grid-community";
import { AgGridReact } from "ag-grid-react";

ModuleRegistry.registerModules([AllCommunityModule]);

export const swayGridTheme = themeQuartz.withParams({
  fontFamily: "var(--font-inter), ui-sans-serif, system-ui, sans-serif",
  fontSize: 13,
  accentColor: "#5a3ff2",
  foregroundColor: "#17141f",
  backgroundColor: "#ffffff",
  headerBackgroundColor: "#f4f1ea",
  headerTextColor: "#3b3646",
  headerFontWeight: 600,
  borderColor: "#e6e1d8",
  rowHoverColor: "#f7f4ee",
  oddRowBackgroundColor: "#fdfcfa",
  wrapperBorderRadius: 14,
  rowHeight: 42,
  headerHeight: 40,
  spacing: 6,
});

export function Grid<T>({ rows, cols, height = 420, quickFilter, options }: { rows: T[]; cols: ColDef<T>[]; height?: number; quickFilter?: string; options?: GridOptions<T> }) {
  return (
    <div style={{ height }} className="w-full min-w-0 max-w-full overflow-hidden">
      <AgGridReact<T>
        theme={swayGridTheme}
        rowData={rows}
        columnDefs={cols}
        defaultColDef={{ sortable: true, filter: true, resizable: true, minWidth: 90, flex: 1 }}
        quickFilterText={quickFilter}
        animateRows
        pagination={rows.length > 50}
        paginationPageSize={50}
        paginationPageSizeSelector={[25, 50, 100]}
        suppressCellFocus
        enableCellTextSelection
        overlayNoRowsTemplate="<span style='color:#6e6878'>Nothing here yet</span>"
        {...options}
      />
    </div>
  );
}

export function sparkline(values: number[] | null | undefined): string {
  if (!values || values.length === 0) return "";
  const w = 80,
    h = 22;
  const min = Math.min(0, ...values),
    max = Math.max(0.01, ...values);
  const x = (i: number) => (values.length === 1 ? w / 2 : (i / (values.length - 1)) * w);
  const y = (v: number) => h - ((v - min) / (max - min)) * (h - 4) - 2;
  const pts = values.map((v, i) => `${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(" ");
  const zero = y(0).toFixed(1);
  return `<svg width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" aria-hidden="true"><line x1="0" x2="${w}" y1="${zero}" y2="${zero}" stroke="#e6e1d8"/><polyline fill="none" stroke="#5a3ff2" stroke-width="1.6" points="${pts}"/></svg>`;
}
