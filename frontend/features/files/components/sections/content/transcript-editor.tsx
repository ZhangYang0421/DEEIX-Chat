"use client";

import * as React from "react";
import { useTranslations } from "next-intl";
import { Save } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
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
      speakerOverrides: { ...(value.document.speakerOverrides ?? {}) },
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

  const setSpeakerName = React.useCallback((speakerID: string, name: string) => {
    setDraft((current) => current ? {
      ...current,
      document: {
        ...current.document,
        speakerNames: { ...current.document.speakerNames, [speakerID]: name },
      },
    } : current);
  }, []);

  const setSegmentText = React.useCallback((segmentID: string, text: string) => {
    setDraft((current) => current ? {
      ...current,
      document: {
        ...current.document,
        segments: current.document.segments.map((segment) => segment.segmentID === segmentID ? { ...segment, text } : segment),
      },
    } : current);
  }, []);

  const setSegmentSpeaker = React.useCallback((segmentID: string, speakerKey: string) => {
    setDraft((current) => {
      if (!current) return current;
      const segment = current.document.segments.find((item) => item.segmentID === segmentID);
      const rawSpeakerKey = segment?.speakerID == null ? "" : String(segment.speakerID);
      const nextOverrides = { ...(current.document.speakerOverrides ?? {}) };
      if (!speakerKey || speakerKey === "unknown" || speakerKey === rawSpeakerKey) {
        delete nextOverrides[segmentID];
      } else {
        nextOverrides[segmentID] = speakerKey;
      }
      return {
        ...current,
        document: {
          ...current.document,
          speakerOverrides: nextOverrides,
        },
      };
    });
  }, []);

  const speakerLabel = React.useCallback((id: string) => t("speaker", { id }), [t]);

  const speakerEntries = React.useMemo(() => {
    if (!draft) return [];
    return Object.entries(draft.document.speakerNames).sort(([left], [right]) => left.localeCompare(right));
  }, [draft?.document.speakerNames, draft]);

  const effectiveSpeakerKeys = React.useMemo(() => {
    if (!draft) return new Set<string>();
    return new Set(draft.document.segments.map((segment) => {
      const rawSpeakerKey = segment.speakerID == null ? "unknown" : String(segment.speakerID);
      return draft.document.speakerOverrides?.[segment.segmentID] ?? rawSpeakerKey;
    }));
  }, [draft?.document.segments, draft?.document.speakerOverrides, draft]);

  const save = async () => {
    if (!saved || !draft) return;
    const speakerNames = Object.fromEntries(
      Object.entries(draft.document.speakerNames).filter(([key, value]) => value.trim() !== (saved.document.speakerNames[key] ?? "").trim()),
    );
    const savedOverrides = saved.document.speakerOverrides ?? {};
    const draftOverrides = draft.document.speakerOverrides ?? {};
    const overrideKeys = new Set([...Object.keys(savedOverrides), ...Object.keys(draftOverrides)]);
    const speakerOverrides = Object.fromEntries(
      [...overrideKeys]
        .filter((segmentID) => (draftOverrides[segmentID] ?? "") !== (savedOverrides[segmentID] ?? ""))
        .map((segmentID) => [segmentID, draftOverrides[segmentID] ?? ""]),
    );
    const savedSegments = new Map(saved.document.segments.map((segment) => [segment.segmentID, segment.text]));
    const segments = draft.document.segments
      .filter((segment) => segment.text.trim() !== (savedSegments.get(segment.segmentID) ?? "").trim())
      .map((segment) => ({ segmentID: segment.segmentID, text: segment.text.trim() }));
    if (Object.keys(speakerNames).length === 0 && Object.keys(speakerOverrides).length === 0 && segments.length === 0) {
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
        speakerOverrides,
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
          <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
            <p className="text-xs font-medium text-muted-foreground">{t("speakerNames")}</p>
            <span className="text-[11px] text-muted-foreground">{t("effectiveSpeakerCount", { count: effectiveSpeakerKeys.size })}</span>
          </div>
          <p className="mb-3 text-[11px] text-muted-foreground">{t("speakerOverrideHint")}</p>
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
          const rawSpeakerKey = segment.speakerID == null ? "" : String(segment.speakerID);
          const selectedSpeakerKey = draft.document.speakerOverrides?.[segment.segmentID] ?? rawSpeakerKey;
          return (
            <TranscriptSegmentRow
              key={segment.segmentID}
              segment={segment}
              speakerEntries={speakerEntries}
              selectedSpeakerKey={selectedSpeakerKey}
              saving={saving}
              unknownSpeakerLabel={t("unknownSpeaker")}
              reviewSuggestedLabel={t("reviewSuggested")}
              editedLabel={t("edited")}
              speakerLabel={speakerLabel}
              onSpeakerChange={setSegmentSpeaker}
              onTextChange={setSegmentText}
            />
          );
        })}
      </section>
    </div>
  );
}

type TranscriptSegmentRowProps = {
  segment: FileTranscriptSegmentDTO;
  speakerEntries: [string, string][];
  selectedSpeakerKey: string;
  saving: boolean;
  unknownSpeakerLabel: string;
  reviewSuggestedLabel: string;
  editedLabel: string;
  speakerLabel: (id: string) => string;
  onSpeakerChange: (segmentID: string, speakerKey: string) => void;
  onTextChange: (segmentID: string, text: string) => void;
};

const TranscriptSegmentRow = React.memo(function TranscriptSegmentRow({
  segment,
  speakerEntries,
  selectedSpeakerKey,
  saving,
  unknownSpeakerLabel,
  reviewSuggestedLabel,
  editedLabel,
  speakerLabel,
  onSpeakerChange,
  onTextChange,
}: TranscriptSegmentRowProps) {
  const handleSpeakerChange = React.useCallback((value: string) => {
    onSpeakerChange(segment.segmentID, value);
  }, [segment.segmentID, onSpeakerChange]);

  const handleTextChange = React.useCallback((event: React.ChangeEvent<HTMLTextAreaElement>) => {
    onTextChange(segment.segmentID, event.target.value);
  }, [segment.segmentID, onTextChange]);

  return (
    <article className="rounded-md border bg-background/65 p-3">
      <div className="mb-2 flex flex-wrap items-center gap-2 text-[11px] text-muted-foreground">
        <span>{formatTimestamp(segment.startMs)}–{formatTimestamp(segment.endMs)}</span>
        <span>·</span>
        <Select
          value={selectedSpeakerKey || "unknown"}
          onValueChange={handleSpeakerChange}
          disabled={saving || speakerEntries.length === 0}
        >
          <SelectTrigger className="h-7 w-[9rem] px-2 text-[11px]">
            <SelectValue placeholder={unknownSpeakerLabel} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="unknown" className="text-[11px]">{unknownSpeakerLabel}</SelectItem>
            {speakerEntries.map(([speakerID, name]) => (
              <SelectItem key={speakerID} value={speakerID} className="text-[11px]">
                {name || speakerLabel(speakerID)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {segment.lowConfidence ? (
          <span className="rounded bg-amber-500/15 px-1.5 py-0.5 text-amber-600 dark:text-amber-400">
            {reviewSuggestedLabel}
          </span>
        ) : null}
        {segment.edited ? (
          <span className="rounded bg-primary/10 px-1.5 py-0.5 text-primary">
            {editedLabel}
          </span>
        ) : null}
      </div>
      <Textarea
        value={segment.text}
        maxLength={20000}
        className="min-h-20 resize-y"
        onChange={handleTextChange}
      />
    </article>
  );
});
