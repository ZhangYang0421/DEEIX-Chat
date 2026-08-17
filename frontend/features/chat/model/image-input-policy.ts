import type { ChatModelOption } from "@/features/chat/types/chat-runtime";
import type { MCPToolDTO } from "@/shared/api/mcp.types";
import { hasSelectedImageAttachmentProcessor } from "@/shared/lib/mcp-tool-selection";

export function modelAllowsImageInput(model: ChatModelOption | null): boolean {
  if (!model || model.inputModalities === null) {
    return true;
  }
  return model.inputModalities.includes("image");
}

export function canAttachRawImages(
  model: ChatModelOption | null,
  selectedToolIDs: number[],
  availableTools: MCPToolDTO[],
): boolean {
  return modelAllowsImageInput(model) || hasSelectedImageAttachmentProcessor(selectedToolIDs, availableTools);
}
