import type { TicketClass, TicketSpec } from './types.js';

/**
 * Plan stage: classify the ticket and produce an implementation plan outline.
 * Classification drives which validation gates apply (FR-PLAN-03).
 */
export interface ImplementationPlan {
  ticket: TicketSpec;
  classification: TicketClass;
  tasks: string[];
}

const MIGRATION_HINTS = [
  /\bmigration\b/i,
  /\bschema\b/i,
  /\badd column\b/i,
  /\bdrop column\b/i,
  /\balter table\b/i,
  /\bcreate table\b/i,
  /\bforeign key\b/i,
  /\bindex on\b/i,
];

const CONSUMER_HINTS = [
  /\bconsumer\b/i,
  /\bqueue\b/i,
  /\bsubscriber\b/i,
  /\bevent handler\b/i,
  /\bkafka\b/i,
  /\brabbitmq\b/i,
  /\bsqs\b/i,
  /\bpublisher\b/i,
  /\bwebhook receiver\b/i,
];

export function classifyTicket(ticket: TicketSpec): TicketClass {
  const text = `${ticket.title}\n${ticket.description}\n${ticket.labels.join(' ')}`;
  const wantsMigration = MIGRATION_HINTS.some((re) => re.test(text));
  const wantsConsumer = CONSUMER_HINTS.some((re) => re.test(text));

  if (wantsMigration) return 'migration-required';
  if (wantsConsumer) return 'consumer-only';
  return 'endpoint-only';
}

/** Spec sufficiency check (FR-TICKET-05): refuse vague tickets with a clarifying question. */
export function checkSpec(ticket: TicketSpec): { sufficient: boolean; question?: string } {
  if (!ticket.title.trim()) {
    return { sufficient: false, question: 'Ticket has no title. What should be delivered?' };
  }
  if (ticket.description.trim().length < 40 && ticket.acceptanceCriteria.length === 0) {
    return {
      sufficient: false,
      question: `Ticket "${ticket.id}" lacks a description or acceptance criteria. Please add expected behavior so this can be implemented autonomously.`,
    };
  }
  return { sufficient: true };
}

export function planFromTicket(ticket: TicketSpec): ImplementationPlan {
  const classification = classifyTicket(ticket);
  const tasks: string[] = [];

  switch (classification) {
    case 'endpoint-only':
      tasks.push('Define route/handler for the new endpoint');
      tasks.push('Add request/response types per repo conventions');
      tasks.push('Write integration tests hitting the endpoint');
      break;
    case 'migration-required':
      tasks.push('Draft up-migration (expand-first: additive changes only)');
      tasks.push('Write down-migration reversing the change');
      tasks.push('Update schema/types generated from DB');
      tasks.push('Implement dependent API changes');
      tasks.push('Add tests covering migrated-schema behavior');
      break;
    case 'consumer-only':
      tasks.push('Define message/event payload contract');
      tasks.push('Implement consumer handler with idempotency guard');
      tasks.push('Register consumer in service bootstrap');
      tasks.push('Add tests including duplicate-delivery case');
      break;
  }

  return { ticket, classification, tasks };
}
