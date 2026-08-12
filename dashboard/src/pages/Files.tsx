import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { client } from "../connectClient";
import type { FileMetadata } from "../gen/agentfleet/v1/files_pb";
import { InlineError } from "../components/InlineError";
import { ConfirmModal } from "../components/ConfirmModal";
import { useMediaQuery } from "../useMediaQuery";
import { formatBytes } from "./Worktrees";

// Fleet-wide shared file space (docs/adr/0030): one flat Garage S3 bucket,
// visible to every session and to the human. core mints short-lived presigned
// URLs; the bytes move directly between this browser and Garage — this page
// never sends file contents through core/gRPC.
//
// The mockups show a "WRITTEN BY" column. There is no provenance to show: ADR
// 0030 makes the object key the filename verbatim, with no per-caller scoping,
// and the upload presign carries no task id. That column is omitted rather than
// faked.

const COLS = "grid-cols-[24px_1fr_110px_170px_150px]";

function relative(iso: string): string {
  if (!iso) return "—";
  const ms = Date.now() - new Date(iso).getTime();
  if (Number.isNaN(ms)) return "—";
  const mins = Math.round(ms / 60_000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  return days === 1 ? "yesterday" : `${days}d ago`;
}

function RowActions({
  file,
  onDeleted,
  onError,
}: {
  file: FileMetadata;
  onDeleted: () => void;
  onError: (msg: string) => void;
}) {
  const [deleting, setDeleting] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const btn = "border border-line px-2.5 py-1 text-[11.5px] text-dim hover:text-base-content disabled:opacity-50";

  async function download() {
    try {
      const res = await client.getFileDownloadUrl({ key: file.key });
      window.open(res.downloadUrl, "_blank", "noreferrer");
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err));
    }
  }

  async function handleDelete() {
    setConfirmOpen(false);
    setDeleting(true);
    try {
      await client.deleteFile({ key: file.key });
      onDeleted();
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err));
      setDeleting(false);
    }
  }

  return (
    <>
      <button type="button" onClick={() => void download()} className={btn}>
        download
      </button>
      <button
        type="button"
        onClick={() => setConfirmOpen(true)}
        disabled={deleting}
        className="border border-line px-2.5 py-1 text-[11.5px] text-dim hover:text-error disabled:opacity-50"
      >
        {deleting ? "…" : "delete"}
      </button>
      <ConfirmModal
        open={confirmOpen}
        message={`Delete ${file.key}? Every session shares this space, so anything still reading it will 404.`}
        confirmLabel="Delete"
        onConfirm={handleDelete}
        onCancel={() => setConfirmOpen(false)}
      />
    </>
  );
}

export function Files() {
  const isDesktop = useMediaQuery("(min-width: 640px)");
  const [files, setFiles] = useState<FileMetadata[]>([]);
  const [loading, setLoading] = useState(true);
  const [uploading, setUploading] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [filter, setFilter] = useState("");
  const [error, setError] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const load = useCallback(() => {
    setLoading(true);
    return client
      .listFiles({})
      .then((res) => setFiles(res.files))
      .catch((err: Error) => setError(err.message))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const upload = useCallback(
    async (selected: File[]) => {
      if (selected.length === 0) return;
      setUploading(true);
      setError(null);
      try {
        for (const file of selected) {
          const res = await client.getFileUploadUrl({ filename: file.name, contentType: file.type });
          const putRes = await fetch(res.uploadUrl, { method: "PUT", body: file });
          if (!putRes.ok) throw new Error(`upload failed for ${file.name}: ${putRes.status}`);
        }
        await load();
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
      } finally {
        setUploading(false);
      }
    },
    [load],
  );

  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return q ? files.filter((f) => f.key.toLowerCase().includes(q)) : files;
  }, [files, filter]);

  const totalBytes = useMemo(() => files.reduce((sum, f) => sum + Number(f.sizeBytes), 0), [files]);

  return (
    <div
      className="flex-1 min-h-0 overflow-y-auto px-3.5 sm:px-4.5 pt-4 sm:pt-5 pb-6 flex flex-col gap-3.5"
      onDragOver={(e) => {
        e.preventDefault();
        setDragging(true);
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(e) => {
        e.preventDefault();
        setDragging(false);
        void upload(Array.from(e.dataTransfer.files));
      }}
    >
      <InlineError message={error} onRetry={load} onDismiss={() => setError(null)} />

      <div className="flex items-baseline gap-3 flex-wrap">
        <h2 className="text-[13.5px] sm:text-[14px] font-semibold">Shared files</h2>
        <span className="text-[11px] sm:text-[11.5px] text-dim2">
          {isDesktop && "one flat space · every session and you can read, write and delete · "}
          {files.length} file{files.length === 1 ? "" : "s"} · {formatBytes(totalBytes)}
        </span>
        <label className="ml-auto flex items-center gap-2 border border-line px-2.5 py-1.5 text-[11.5px] text-dim2 focus-within:border-primary/60">
          <span aria-hidden>⌕</span>
          <input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="filter files"
            aria-label="filter files"
            className="bg-transparent outline-none min-w-0 w-32 text-base-content placeholder:text-dim2"
          />
        </label>
      </div>

      <input
        ref={fileInputRef}
        type="file"
        multiple
        onChange={(e) => {
          const picked = Array.from(e.target.files ?? []);
          e.target.value = "";
          void upload(picked);
        }}
        className="hidden"
      />
      <button
        type="button"
        onClick={() => fileInputRef.current?.click()}
        disabled={uploading}
        className={`border border-dashed px-4 py-4 sm:py-5 text-center ${
          dragging ? "border-primary bg-primary/5" : "border-acc-line"
        }`}
      >
        <div className="text-[12px] sm:text-[12.5px] text-dim">
          {uploading ? "uploading…" : isDesktop ? "drop files here — or " : "upload a file"}
          {!uploading && isDesktop && <span className="text-primary">choose from your machine</span>}
        </div>
        <div className="text-[10.5px] sm:text-[11px] text-dim2 mt-1.5">
          {isDesktop ? (
            <>
              an agent reads them with <span className="text-dim">list_shared_files</span> /{" "}
              <span className="text-dim">get_shared_file_download_url</span>
            </>
          ) : (
            "shared with every session"
          )}
        </div>
      </button>

      {!loading && files.length === 0 && !error && (
        <div className="text-[12.5px] text-dim2">No files in the shared space yet.</div>
      )}
      {files.length > 0 && shown.length === 0 && (
        <div className="text-[12.5px] text-dim2">No file matches “{filter}”.</div>
      )}

      {shown.length > 0 &&
        (isDesktop ? (
          <div className="border border-line2">
            <div className={`grid ${COLS} gap-3.5 px-3.5 py-2 border-b border-line text-[10.5px] tracking-[0.1em] text-dim2`}>
              <div />
              <div>NAME</div>
              <div>SIZE</div>
              <div>WHEN</div>
              <div />
            </div>
            {shown.map((f, i) => (
              <div
                key={f.key}
                className={`grid ${COLS} gap-3.5 px-3.5 py-2.5 items-center ${
                  i === shown.length - 1 ? "" : "border-b border-line3"
                }`}
              >
                <span className="text-[12px] text-dim2" aria-hidden>
                  ▤
                </span>
                <div className="text-[12.5px] min-w-0 truncate" title={f.key}>
                  {f.key}
                </div>
                <div className="text-[12px] text-dim">{formatBytes(f.sizeBytes)}</div>
                <div className="text-[12px] text-dim">{relative(f.lastModified)}</div>
                <div className="flex gap-1.5 justify-end">
                  <RowActions file={f} onDeleted={load} onError={setError} />
                </div>
              </div>
            ))}
          </div>
        ) : (
          shown.map((f) => (
            <div key={f.key} className="border border-line2 px-3.5 py-3">
              <div className="flex items-baseline gap-2">
                <span className="text-[12.5px] min-w-0 truncate" title={f.key}>
                  {f.key}
                </span>
                <span className="text-[11px] text-dim2 ml-auto flex-none">{formatBytes(f.sizeBytes)}</span>
              </div>
              <div className="flex items-center gap-2 mt-2">
                <span className="text-[11px] text-dim2">{relative(f.lastModified)}</span>
                <div className="ml-auto flex gap-1.5">
                  <RowActions file={f} onDeleted={load} onError={setError} />
                </div>
              </div>
            </div>
          ))
        ))}
    </div>
  );
}
