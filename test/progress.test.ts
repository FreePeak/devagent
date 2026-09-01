import { describe, expect, it } from 'vitest';
import { isNdjsonProgressLine, isPureThinkingLine } from '../src/workers/progress.js';

describe('progress classifier (PRD Q33)', () => {
  describe('isPureThinkingLine', () => {
    it('returns true for thinking_delta lines', () => {
      expect(isPureThinkingLine('{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":" = \""}}')).toBe(true);
    });
    it('returns true for thinking_start lines', () => {
      expect(isPureThinkingLine('{"type":"message_update","assistantMessageEvent":{"type":"thinking_start"}}')).toBe(true);
    });
    it('does not flag tool or text lines', () => {
      expect(isPureThinkingLine('{"type":"tool_execution_start","toolName":"bash"}')).toBe(false);
      expect(isPureThinkingLine('{"type":"message_update","assistantMessageEvent":{"type":"text_end","content":"hi"}}')).toBe(false);
    });
  });

  describe('isNdjsonProgressLine', () => {
    it('progress: tool_execution_start', () => {
      expect(isNdjsonProgressLine('{"type":"tool_execution_start","toolName":"read","args":{}}')).toBe(true);
    });
    it('progress: tool_execution_end', () => {
      expect(isNdjsonProgressLine('{"type":"tool_execution_end","toolName":"bash","result":{}}')).toBe(true);
    });
    it('progress: text_end (answer turn completed)', () => {
      expect(isNdjsonProgressLine('{"type":"message_update","assistantMessageEvent":{"type":"text_end","content":"hi"}}')).toBe(true);
    });
    it('progress: toolcall_start (tool arg assembled)', () => {
      expect(isNdjsonProgressLine('{"type":"message_update","assistantMessageEvent":{"type":"toolcall_start","id":"c1","toolName":"bash"}}')).toBe(true);
    });
    it('not progress: thinking_delta', () => {
      expect(isNdjsonProgressLine('{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":" x"}}')).toBe(false);
    });
    it('not progress: empty line', () => {
      expect(isNdjsonProgressLine('')).toBe(false);
      expect(isNdjsonProgressLine('   ')).toBe(false);
    });
    it('not progress: arbitrary non-progress JSON', () => {
      expect(isNdjsonProgressLine('{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":" hi"}}')).toBe(false);
      expect(isNdjsonProgressLine('{"type":"session","id":"abc"}')).toBe(false);
    });
  });
});
