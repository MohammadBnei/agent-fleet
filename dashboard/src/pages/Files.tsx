import { useCallback, useEffect, useRef, useState } from "react";
import { client } from "../connectClient";
import type { FileMetadata } from "../gen/agentfleet/v1/files_pb";
import { ErrorModal } from "../components/ErrorModal";
import { ConfirmModal } from "../components/ConfirmModal";

// Fleet-wide shared file space (docs/adr/0030): one flat Garage S3 bucket,
// visible to every task and to the human. core mints short-lived presigned
// URLs; the actual bytes move directly between this browser and Garage —
// this page never sends file contents through core/gRPC.

function formatSize(bytes: bigint): string {
  const n = Number(bytes);
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

function FileRow({ file, onDeleted }: { file: FileMetadata; onDeleted: () => void }) {
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);

  async function handleDownload() {
    try {
      const res = await client.getFileDownloadUrl({ key: file.key });
      window.open(res.downloadUrl, "_blank", "noreferrer");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }

  async function handleDelete() {
    setConfirmOpen(false);
    setDeleting(true);
    setError(null);
    try {
      await client.deleteFile({ key: file.key });
      onDeleted();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setDeleting(false);
    }
  }

  return (
    <tr>
      <td className="text-[11px] font-mono">{file.key}</td>
      <td className="text-[11px]">{formatSize(file.sizeBytes)}</td>
      <td className="text-[11px]">{file.lastModified ? new Date(file.lastModified).toLocaleString() : "—"}</td>
      <td className="text-right align-top">
        {error && <div className="text-error text-[10px] mb-1">{error}</div>}
        <button type="button" onClick={handleDownload} className="btn btn-xs mr-1">
          Download
        </button>
        <button type="button" onClick={() => setConfirmOpen(true)} disabled={deleting} className="btn btn-xs btn-error">
          {deleting ? "Deleting…" : "Delete"}
        </button>
        <ConfirmModal
          open={confirmOpen}
          message={`Delete ${file.key}?`}
          onConfirm={handleDelete}
          onCancel={() => setConfirmOpen(false)}
        />
      </td>
    </tr>
  );
}

// onBack is only passed by the mobile wrapper — same convention as
// Worktrees.tsx's onBack.
export function Files({ onBack }: { onBack?: () => void } = {}) {
  const [files, setFiles] = useState<FileMetadata[]>([]);
  const [loading, setLoading] = useState(true);
  const [uploading, setUploading] = useState(false);
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

  async function handleUpload(e: React.ChangeEvent<HTMLInputElement>) {
    const selected = e.target.files?.[0];
    e.target.value = "";
    if (!selected) return;
    setUploading(true);
    setError(null);
    try {
      const res = await client.getFileUploadUrl({
        filename: selected.name,
        contentType: selected.type,
      });
      const putRes = await fetch(res.uploadUrl, { method: "PUT", body: selected });
      if (!putRes.ok) throw new Error(`upload failed: ${putRes.status}`);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setUploading(false);
    }
  }

  return (
    <div className="lg:col-span-2 min-h-0 overflow-y-auto p-4 overflow-x-auto">
      <div className="flex items-center gap-3 mb-3">
        {onBack && (
          <button type="button" onClick={onBack} className="text-[17px] text-base-content/60 w-7 h-8 flex items-center">
            ‹
          </button>
        )}
        <h2 className="font-semibold text-base">Files</h2>
        <button type="button" onClick={load} disabled={loading} className="btn btn-xs">
          {loading ? "Refreshing…" : "Refresh"}
        </button>
        <input ref={fileInputRef} type="file" onChange={handleUpload} className="hidden" />
        <button
          type="button"
          onClick={() => fileInputRef.current?.click()}
          disabled={uploading}
          className="btn btn-xs btn-primary"
        >
          {uploading ? "Uploading…" : "Upload"}
        </button>
      </div>
      <ErrorModal message={error} onClose={() => setError(null)} />
      {!loading && files.length === 0 && !error && (
        <div className="opacity-60 text-sm">No files in the shared space yet.</div>
      )}
      {files.length > 0 && (
        <table className="table table-sm">
          <thead>
            <tr className="text-[10px]">
              <th>Key</th>
              <th>Size</th>
              <th>Modified</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {files.map((f) => (
              <FileRow key={f.key} file={f} onDeleted={load} />
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
