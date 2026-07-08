// Табличная основа на TanStack Table. Таблица живёт внутри карточки и
// скроллится сама (overflow-x) — страница не получает горизонтальный
// скролл до 1280px. Раскрытие строки — renderExpanded + expandedId.

import {
  flexRender,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table';
import type { ColumnDef } from '@tanstack/react-table';
import type { ReactNode } from 'react';
import { EmptyState } from './ui';

interface DataTableProps<T> {
  columns: ColumnDef<T, unknown>[];
  data: T[];
  rowId: (row: T) => string;
  empty: ReactNode;
  expandedId?: string | null;
  onRowClick?: (row: T) => void;
  /** Доступная подпись кликабельной строки (aria-label для клавиатуры/SR). */
  rowLabel?: (row: T) => string;
  renderExpanded?: (row: T) => ReactNode;
}

export function DataTable<T>({
  columns,
  data,
  rowId,
  empty,
  expandedId,
  onRowClick,
  rowLabel,
  renderExpanded,
}: DataTableProps<T>) {
  const table = useReactTable({
    data,
    columns,
    getRowId: (row) => rowId(row),
    getCoreRowModel: getCoreRowModel(),
  });

  if (data.length === 0) {
    return <EmptyState>{empty}</EmptyState>;
  }

  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[640px] border-collapse text-sm">
        <thead>
          {table.getHeaderGroups().map((hg) => (
            <tr key={hg.id} className="border-b border-line">
              {hg.headers.map((h) => (
                <th
                  key={h.id}
                  className="px-4 py-2.5 text-left text-xs font-medium tracking-wide text-muted uppercase"
                >
                  {h.isPlaceholder ? null : flexRender(h.column.columnDef.header, h.getContext())}
                </th>
              ))}
            </tr>
          ))}
        </thead>
        <tbody>
          {table.getRowModel().rows.map((row) => {
            const id = row.id;
            const expanded = expandedId === id && renderExpanded !== undefined;
            return (
              <RowGroup key={id}>
                <tr
                  className={`border-b border-line transition-colors last:border-0 ${
                    onRowClick !== undefined ? 'cursor-pointer hover:bg-paper focus-visible:bg-paper' : ''
                  } ${expanded ? 'bg-paper' : ''}`}
                  onClick={onRowClick !== undefined ? () => onRowClick(row.original) : undefined}
                  role={onRowClick !== undefined ? 'button' : undefined}
                  tabIndex={onRowClick !== undefined ? 0 : undefined}
                  aria-expanded={renderExpanded !== undefined && onRowClick !== undefined ? expanded : undefined}
                  aria-label={onRowClick !== undefined && rowLabel !== undefined ? rowLabel(row.original) : undefined}
                  onKeyDown={
                    onRowClick !== undefined
                      ? (e) => {
                          if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault();
                            onRowClick(row.original);
                          }
                        }
                      : undefined
                  }
                >
                  {row.getVisibleCells().map((cell) => (
                    <td key={cell.id} className="px-4 py-2.5 align-middle">
                      {flexRender(cell.column.columnDef.cell, cell.getContext())}
                    </td>
                  ))}
                </tr>
                {expanded && (
                  <tr className="border-b border-line last:border-0">
                    <td colSpan={row.getVisibleCells().length} className="bg-paper px-4 pt-0 pb-3">
                      {renderExpanded(row.original)}
                    </td>
                  </tr>
                )}
              </RowGroup>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// Фрагмент-обёртка: пара строк (строка + раскрытие) под одним key.
function RowGroup({ children }: { children: ReactNode }) {
  return <>{children}</>;
}
