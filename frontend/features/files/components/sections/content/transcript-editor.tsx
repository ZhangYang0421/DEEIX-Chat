"use client";

import * as React from "react";
import { useTranslations } from "next-intl";
import { Save } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { useLocalizedErrorMessage } from "@/i18n/use-localized-error";
import { getFileTranscript, patchFileTranscript } from "@/shared/api/file";
import type { FileTranscriptDTO, FileTranscriptSegmentDTO } from "@/shared/api/file.types";
import { resolveAccessToken } from "@/shared/auth/resolve-access-token";
import { ApiError } from "@/shared/api/http-client";

function formatTimestamp(ms: number): string {
  const seconds = Math.max(0, Math.floor(ms / 1000));
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const remaining = seconds % 60;
  return hours > 0
    ? `${String(hours).padStart(2, "0")}:${String(minutes).padStart(2, "0")}:${String(remaining).padStart(2, "0")}`
    : `${String(minutes).padStart(2, "0")}:${String(remaining).padStart(2, "0")}`;
}

function cloneTranscript(value: FileTranscriptDTO): FileTranscriptDTO {
  return {
    ...value,
    document: {
      ...value.document,
      speakerNames: { ...value.document.speakerNames },
      segments: value.document.segments.map((segment) => ({ ...segment })),
    },
  };
}

export function TranscriptEditor({ fileID }: { fileID: string }) {
  const t = useTranslations("files.transcript");
  const resolveErrorMessage = useLocalizedErrorMessage();
  const [saved, setSaved] = React.useState<FileTranscriptDTO | null>(null);
  const [draft, setDraft] = React.useState<FileTranscriptDTO | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const token = await resolveAccessToken();
      if (!token) throw new Error(t("signInRequired"));
      const result = await getFileTranscript(token, fileID);
      setSaved(cloneTranscript(result));
      setDraft(cloneTranscript(result));
    } catch (error) {
      toast.error(t("loadFailed"), { description: resolveErrorMessage(error, t("retryLater")) });
      setSaved(null);
      setDraft(null);
    } finally {
      setLoading(false);
    }
  }, [fileID, resolveErrorMessage, t]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const setSpeakerName = (speakerID: string, name: string) => {
    setDraft((current) => current ? {
      ...current,
      document: {
        ...current.document,
        speakerNames: { ...current.document.speakerNames, [speakerID]: name },
      },
    } : current);
  };

  const setSegmentText = (segmentID: string, text: string) => {
    setDraft((current) => current ? {
      ...current,
      document: {
        ...current.document,
        segments: current.document.segments.map((segment) => segment.segmentID === segmentID ? { ...segment, text } : segment),
      },
    } : current);
  };

  const save = async () => {
    if (!saved || !draft) return;
    const speakerNames = Object.fromEntries(
      Object.entries(draft.document.speakerNames).filter(([key, value]) => value.trim() !== (saved.document.speakerNames[key] ?? "").trim()),
    );
    const savedSegments = new Map(saved.document.segments.map((segment) => [segment.segmentID, segment.text]));
    const segments = draft.document.segments
      .filter((segment) => segment.text.trim() !== (savedSegments.get(segment.segmentID) ?? "").trim())
      .map((segment) => ({ segmentID: segment.segmentID, text: segment.text.trim() }));
    if (Object.keys(speakerNames).length === 0 && segments.length === 0) {
      toast.info(t("nothingToSave"));
      return;
    }
    if (Object.values(speakerNames).some((name) => !name.trim()) || segments.some((segment) => !segment.text)) {
      toast.error(t("invalidEdit"));
      return;
    }
    setSaving(true);
    try {
      const token = await resolveAccessToken();
      if (!token) throw new Error(t("signInRequired"));
      const result = await patchFileTranscript(token, fileID, {
        revision: saved.document.revision,
        speakerNames,
        segments,
      });
      setSaved(cloneTranscript(result));
      setDraft(cloneTranscript(result));
      toast.success(t("saved"));
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        toast.error(t("revisionConflict"), { description: t("revisionConflictDetail") });
      } else {
        toast.error(t("saveFailed"), { description: resolveErrorMessage(error, t("retryLater")) });
      }
    } finally {
      setSaving(false);
    }
  };

  if (loading) {
    return <div className="flex min-h-48 items-center justify-center text-sm text-muted-foreground">{t("loading")}</div>;
  }
  if (!draft) {
    return <div className="flex min-h-48 items-center justify-center text-sm text-muted-foreground">{t("unavailable")}</div>;
  }

  const speakerEntries = Object.entries(draft.document.speakerNames).sort(([left], [right]) => left.localeCompare(right));

  return (
    <div className="space-y-4 pb-4">
      <div className="sticky top-0 z-10 flex flex-wrap items-center justify-between gap-3 rounded-md border bg-background/95 p-3 backdrop-blur">
        <div>
          <p className="text-sm font-medium">{t("title")}</p>
          <p className="text-xs text-muted-foreground">{t("revision", { revision: draft.document.revision })}</p>
        </div>
        <Button size="sm" onClick={() => void save()} disabled={saving}>
          <Save className="size-4" />
          {saving ? t("saving") : t("save")}
        </Button>
      </div>

      {speakerEntries.length > 0 ? (
        <section className="rounded-md border bg-background/65 p-3">
          <p className="mb-2 text-xs font-medium text-muted-foreground">{t("speakerNames")}</p>
          <div className="grid gap-2 sm:grid-cols-2">
            {speakerEntries.map(([speakerID, name]) => (
              <label key={speakerID} className="grid grid-cols-[5rem_minmax(0,1fr)] items-center gap-2 text-xs">
                <span className="text-muted-foreground">{t("speaker", { id: speakerID })}</span>
                <Input value={name} maxLength={80} onChange={(event) => setSpeakerName(speakerID, event.target.value)} />
              </label>
            ))}
          </div>
        </section>
      ) : null}

      <section className="space-y-2">
        {draft.document.segments.map((segment: FileTranscriptSegmentDTO) => {
          const speakerKey = segment.speakerID == null ? "" : String(segment.speakerID);
          const speakerName = draft.document.speakerNames[speakerKey] || t("unknownSpeaker");
          return (
            <article key={segment.segmentID} className="rounded-md border bg-background/65 p-3">
              <div className="mb-2 flex flex-wrap items-center gap-2 text-[11px] text-muted-foreground">
                <span>{formatTimestamp(segment.startMs)}–{formatTimestamp(segment.endMs)}</span>
                <span>·</span>
                <span>{speakerName}</span>
                {segment.lowConfidence ? <span className="rounded bg-amber-500/15 px-1.5 py-0.5 text-amber-600 dark:text-amber-400">{t("reviewSuggested")}</span> : null}
                {segment.edited ? <span className="rounded bg-primary/10 px-1.5 py-0.5 text-primary">{t("edited")}</span> : null}
              </div>
              <Textarea
                value={segment.text}
                maxLength={20000}
                className="min-h-20 resize-y"
                onChange={(event) => setSegmentText(segment.segmentID, event.target.value)}
              />
            </article>
          );
        })}
      </section>
    </div>
  );
}
