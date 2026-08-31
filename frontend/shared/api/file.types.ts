import type {
  DeleteFileResponse,
  FileEmbeddingSubmissionResponse,
  FileListResponse,
  FileObjectResponse,
  FileProcessingStatusResponse,
  FileUploadResponse,
  StorageQuotaResponse,
} from "@deeix/api-contract";

export type FileObjectDTO = FileObjectResponse;

export type FileTranscriptSegmentDTO = {
  segmentID: string;
  startMs: number;
  endMs: number;
  speakerID: number | null;
  text: string;
  originalText: string;
  avgConfidence?: number;
  lowConfidence?: boolean;
  edited?: boolean;
};

export type FileTranscriptDTO = {
  fileID: string;
  document: {
    version: number;
    revision: number;
    source: string;
    model: string;
    fileID?: string;
    fileName?: string;
    durationMs?: number;
    speakerNames: Record<string, string>;
    speakerOverrides?: Record<string, string>;
    segments: FileTranscriptSegmentDTO[];
    updatedAt?: string;
  };
};

export type FileProcessingStatusDTO = FileProcessingStatusResponse;
export type FileEmbeddingSubmissionDTO = FileEmbeddingSubmissionResponse;

export type FileExtractDTO = {
  fileID: string;
  extractText: string;
  previewText: string;
  extractChars: number;
  extractPages: number;
  ocrUsed: boolean;
};

export type ChatFilePolicyDTO = {
  maxMessageFiles: number;
  maxUploadFileBytes: number;
  allowedMIMETypes: string[];
  imageMaxBytes: number;
  docMaxBytes: number;
  audioMaxBytes: number;
  effectiveImageMaxBytes: number;
  effectiveDocMaxBytes: number;
  effectiveAudioMaxBytes: number;
  fullContextMaxBytes: number;
  fullContextMaxTokens: number;
  fullContextPDFMaxPages: number;
  ragAvailable: boolean;
  ragAvailabilityReason: string;
  capabilityMode: "full_context_only" | "full_context_and_rag";
  fileMode: "auto" | "full_context" | "rag";
};

export type UserStorageQuotaDTO = Omit<StorageQuotaResponse, "id">;

export type FileListResult = Omit<FileListResponse, "quota" | "results"> & {
  results: FileObjectDTO[];
  quota: UserStorageQuotaDTO;
};

export type UploadFileResult = Omit<FileUploadResponse, "file" | "quota"> & {
  file: FileObjectDTO;
  quota: UserStorageQuotaDTO;
};

export type DeleteFileResult = Omit<DeleteFileResponse, "quota"> & {
  quota: UserStorageQuotaDTO;
};
