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
  statsCacheReadsText,
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

/**
 * Column widths every Analytics table shares, so the columns line up from table
 * to table; Duration takes the rest. A chain table splits the name width into
 * its stage number and stage name, so its Runs column lines up too.
 */
export const COLUMN_WIDTH = {
  name: "w-[240px]",
  nameText: "max-w-[208px]",
  stageNumber: "w-[48px]",
  stage: "w-[192px]",
  stageText: "max-w-[160px]",
  entryStageText: "max-w-[96px]",
  runs: "w-[112px]",
  measure: "w-[112px]",
  turns: "w-[144px]",
  figure: "w-[176px]",
} as const;

function durationText(duration: Duration): string {
  if (duration.samples === 0) {
    return "unknown";
  }
  const low = fmtDuration(duration.p50);
  const high = fmtDuration(duration.p95);
  return low === high ? low : `${low} to ${high}`;
}

function tokensTitle(row: Group, tier: string): string | undefined {
  const cacheReads = statsCacheReadsText(row, tier);
  const parts = [
    cacheReads === null ? null : `${cacheReads} cache reads`,
    statsTokensShortfall(row),
  ].filter((part) => part !== null);
  return parts.length === 0 ? undefined : parts.join(", ");
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
      value: (row: Row) => (isRecorded && row.tokens !== null ? row.tokens.uncached : null),
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
      <Table title={title} className="table-fixed">
        <TableHeader>
          {nameColumns.map((column, index) => (
            <SortableHead
              key={column.header}
              columnId={`name${index}`}
              sort={sort}
              className={COLUMN_WIDTH.name}
            >
              {column.header}
            </SortableHead>
          ))}
          <SortableHead columnId="runs" sort={sort} align="right" className={COLUMN_WIDTH.runs}>
            Runs
          </SortableHead>
          <SortableHead columnId={measure} sort={sort} className={COLUMN_WIDTH.measure}>
            {measure === "share" ? "Share" : "Success"}
          </SortableHead>
          <SortableHead columnId="duration" sort={sort} title="p50 to p95">
            Duration
          </SortableHead>
          <SortableHead columnId="turns" sort={sort} align="right" className={COLUMN_WIDTH.turns}>
            Mean turns
          </SortableHead>
          <SortableHead
            columnId="tokens"
            sort={sort}
            align="right"
            title="Cache reads not included"
            className={COLUMN_WIDTH.figure}
          >
            Tokens
          </SortableHead>
          {hasRates ? (
            <SortableHead columnId="cost" sort={sort} align="right" className={COLUMN_WIDTH.figure}>
              Est. cost
            </SortableHead>
          ) : undefined}
        </TableHeader>
        <TableBody ref={body}>
          {ordered.slice(0, shown).map((row, index) => (
            <TableRow key={index} tabIndex={-1}>
              {nameColumns.map((column) => (
                <TableCell key={column.header}>
                  <span
                    title={column.sortText(row)}
                    className={`block truncate ${COLUMN_WIDTH.nameText}`}
                  >
                    {column.cell(row)}
                  </span>
                </TableCell>
              ))}
              <TableCell look="number">{fmtCount(row.runs)}</TableCell>
              <TableCell>
                {fmtPercent(measure === "share" ? row.share : row.success_rate)}
              </TableCell>
              <TableCell>{durationText(row.duration_seconds)}</TableCell>
              <TableCell look="number">
                {row.mean_turns === null ? "not recorded" : fmtMean(row.mean_turns)}
              </TableCell>
              <TableCell look="number" title={tokensTitle(row, tier)}>
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
