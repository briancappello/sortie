import { useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import {
  SortableHead,
  Table,
  TableBody,
  TableCell,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useTableSort } from "@/components/use-table-sort";
import {
  statsCostText,
  statsShortfall,
  statsTokensShortfall,
  statsTokensText,
} from "@/lib/figures";
import { fmtCount, fmtDuration, fmtMean, fmtPercent } from "@/lib/format";

import type { Duration, Group } from "@/api/types.gen";
import type { SortColumn } from "@/components/use-table-sort";
import type { ReactNode } from "react";

const PAGE_ROWS = 20;

function durationText(duration: Duration): string {
  if (duration.samples === 0) {
    return "unknown";
  }
  const low = fmtDuration(duration.p50);
  const high = fmtDuration(duration.p95);
  return low === high ? low : `${low} to ${high}`;
}

/** One name column of a {@link GroupTable}. */
export interface NameColumn<Row> {
  header: string;
  cell: (row: Row) => ReactNode;
  /** The words the cell shows, which the column sorts by. */
  sortText: (row: Row) => string;
}

/** Props of {@link GroupTable}. */
export interface GroupTableProps<Row extends Group> {
  title: string;
  /** The address param that keeps this table's sort. */
  sortParam: string;
  nameColumns: readonly NameColumn<Row>[];
  rows: readonly Row[];
  /** The report's `schema_tier`. */
  tier: string;
  /** Whether the workflow configures `token_rates`, which adds the cost column. */
  hasRates: boolean;
  /** `share` swaps the success column for the share of runs, as a breakdown by run status needs. */
  measure?: "success" | "share";
}

function sortColumns<Row extends Group>(
  nameColumns: readonly NameColumn<Row>[],
  tier: string,
  measure: "success" | "share",
): SortColumn<Row>[] {
  const isRecorded = tier !== "base";
  return [
    ...nameColumns.map((column, index) => ({ id: `name${index}`, value: column.sortText })),
    { id: "runs", value: (row: Row) => row.runs },
    {
      id: measure,
      value: (row: Row) => (measure === "share" ? row.share : row.success_rate),
    },
    {
      id: "duration",
      value: (row: Row) => (row.duration_seconds.samples === 0 ? null : row.duration_seconds.p50),
    },
    { id: "turns", value: (row: Row) => row.mean_turns },
    {
      id: "tokens",
      value: (row: Row) => (isRecorded && row.tokens !== null ? row.tokens.total : null),
    },
    { id: "cost", value: (row: Row) => (isRecorded ? row.cost_usd : null) },
  ];
}

/**
 * Renders one breakdown of a report: name columns, then the figure columns,
 * 20 rows at a time, sortable by any column. The shown count lives in state, so
 * mount the table with a key that changes with the range.
 */
export function GroupTable<Row extends Group>({
  title,
  sortParam,
  nameColumns,
  rows,
  tier,
  hasRates,
  measure = "success",
}: GroupTableProps<Row>) {
  const [shown, setShown] = useState(PAGE_ROWS);
  const body = useRef<HTMLTableSectionElement>(null);
  const focusRow = useRef<number | null>(null);
  const { rows: ordered, sort } = useTableSort(
    rows,
    sortColumns(nameColumns, tier, measure),
    sortParam,
  );

  useEffect(() => {
    if (focusRow.current !== null) {
      body.current?.rows[focusRow.current]?.focus();
      focusRow.current = null;
    }
  }, [shown]);

  function handleShowMore() {
    const next = shown + PAGE_ROWS;
    // The button leaves with the last rows, so focus moves to the first row added.
    if (next >= rows.length) {
      focusRow.current = shown;
    }
    setShown(next);
  }

  return (
    <section className="flex flex-col gap-sm">
      <Table title={title}>
        <TableHeader>
          {nameColumns.map((column, index) => (
            <SortableHead key={column.header} columnId={`name${index}`} sort={sort}>
              {column.header}
            </SortableHead>
          ))}
          <SortableHead columnId="runs" sort={sort}>
            Runs
          </SortableHead>
          <SortableHead columnId={measure} sort={sort}>
            {measure === "share" ? "Share" : "Success"}
          </SortableHead>
          <SortableHead columnId="duration" sort={sort} title="p50 to p95">
            Duration
          </SortableHead>
          <SortableHead columnId="turns" sort={sort} align="right">
            Mean turns
          </SortableHead>
          <SortableHead columnId="tokens" sort={sort} align="right">
            Tokens
          </SortableHead>
          {hasRates ? (
            <SortableHead columnId="cost" sort={sort} align="right">
              Est. cost
            </SortableHead>
          ) : undefined}
        </TableHeader>
        <TableBody ref={body}>
          {ordered.slice(0, shown).map((row, index) => (
            <TableRow key={index} tabIndex={-1}>
              {nameColumns.map((column) => (
                <TableCell key={column.header}>{column.cell(row)}</TableCell>
              ))}
              <TableCell>{fmtCount(row.runs)}</TableCell>
              <TableCell>
                {fmtPercent(measure === "share" ? row.share : row.success_rate)}
              </TableCell>
              <TableCell>{durationText(row.duration_seconds)}</TableCell>
              <TableCell look="number">
                {row.mean_turns === null ? "not recorded" : fmtMean(row.mean_turns)}
              </TableCell>
              <TableCell look="number" title={statsTokensShortfall(row) ?? undefined}>
                {statsTokensText(row, tier)}
              </TableCell>
              {hasRates ? (
                <TableCell look="number" title={statsShortfall(row) ?? undefined}>
                  {statsCostText(row, tier)}
                </TableCell>
              ) : undefined}
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {shown < rows.length ? (
        <div>
          <Button onClick={handleShowMore}>Show more</Button>
        </div>
      ) : undefined}
    </section>
  );
}
