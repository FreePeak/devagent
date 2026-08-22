import { describe, expect, it } from 'vitest';
import { analyzeMigrations, type MigrationFile } from '../src/validation/migration-rules.js';

function ids(findings: { ruleId: string }[]): string[] {
  return findings.map((f) => f.ruleId);
}

function one(sql: string, path = 'migrations/001_up.sql'): MigrationFile {
  return { path, sql };
}

describe('DA001 destructive operations', () => {
  it('fires on DROP TABLE', () => {
    const f = analyzeMigrations([one('DROP TABLE users;')]);
    expect(ids(f)).toContain('DA001');
    expect(f.find((x) => x.ruleId === 'DA001')?.severity).toBe('critical');
  });

  it('does not fire when DROP TABLE appears inside a string literal', () => {
    const f = analyzeMigrations([
      one("INSERT INTO audit_log (msg) VALUES ('planned DROP TABLE users later');"),
    ]);
    expect(f).toHaveLength(0);
  });
});

describe('DA002 column type narrowing', () => {
  it('fires on int to smallint', () => {
    const f = analyzeMigrations([
      one('ALTER TABLE orders ALTER COLUMN qty TYPE smallint;'),
    ]);
    expect(ids(f)).toContain('DA002');
  });

  it('does not fire on widening to bigint or same-size change', () => {
    const f = analyzeMigrations([
      one('ALTER TABLE orders ALTER COLUMN qty TYPE bigint;'),
      one('ALTER TABLE orders ALTER COLUMN note TYPE text;'),
    ]);
    expect(f.filter((x) => x.ruleId === 'DA002')).toHaveLength(0);
  });

  it('fires on varchar shrink within the file set', () => {
    const sql = [
      'CREATE TABLE users (name varchar(255));',
      'ALTER TABLE users ALTER COLUMN name TYPE varchar(50);',
    ].join('\n');
    const f = analyzeMigrations([one(sql)]);
    expect(f.filter((x) => x.ruleId === 'DA002').length).toBeGreaterThanOrEqual(1);
  });
});

describe('DA003 CREATE INDEX without CONCURRENTLY', () => {
  it('fires for plain CREATE INDEX in a postgres file', () => {
    const f = analyzeMigrations([one('CREATE INDEX idx_users_email ON users (email);')]);
    expect(ids(f)).toContain('DA003');
  });

  it('does not fire with CONCURRENTLY or in generic dialect', () => {
    const a = analyzeMigrations([
      one('CREATE INDEX CONCURRENTLY idx_users_email ON users (email);'),
    ]);
    expect(a.filter((x) => x.ruleId === 'DA003')).toHaveLength(0);
    const b = analyzeMigrations(
      [one('CREATE INDEX idx_x ON t (c);')],
      { dialect: 'generic' }
    );
    expect(b.filter((x) => x.ruleId === 'DA003')).toHaveLength(0);
  });
});

describe('DA004 foreign key without index', () => {
  it('fires when referencing column has no index anywhere in the set', () => {
    const f = analyzeMigrations([
      one(
        'ALTER TABLE orders ADD CONSTRAINT fk_customer FOREIGN KEY (customer_id) REFERENCES customers (id);'
      ),
    ]);
    expect(ids(f)).toContain('DA004');
  });

  it('passes when an index exists on the referencing column in another file', () => {
    const files: MigrationFile[] = [
      one('ALTER TABLE orders ADD FOREIGN KEY (customer_id) REFERENCES customers (id);'),
      { path: 'migrations/002_idx.sql', sql: 'CREATE INDEX idx_orders_customer ON orders (customer_id);' },
    ];
    const f = analyzeMigrations(files);
    // DA003 fires on the second file but DA004 must not
    expect(f.filter((x) => x.ruleId === 'DA004')).toHaveLength(0);
  });
});

describe('DA005 NOT NULL without DEFAULT', () => {
  it('fires on ADD COLUMN ... NOT NULL without DEFAULT', () => {
    const f = analyzeMigrations([
      one('ALTER TABLE users ADD COLUMN status varchar(32) NOT NULL;'),
    ]);
    expect(ids(f)).toContain('DA005');
  });

  it('does not fire when DEFAULT is present', () => {
    const f = analyzeMigrations([
      one("ALTER TABLE users ADD COLUMN status varchar(32) NOT NULL DEFAULT 'active';"),
    ]);
    expect(f.filter((x) => x.ruleId === 'DA005')).toHaveLength(0);
  });
});

describe('DA006 missing down-migration', () => {
  it('flags files without a matching down file', () => {
    const f = analyzeMigrations([{ path: 'migrations/007_add_index.up.sql', sql: 'SELECT 1;' }], {
      downMigrations: [],
    });
    expect(ids(f)).toContain('DA006');
  });

  it('does not flag when matching down file is provided', () => {
    const f = analyzeMigrations([{ path: 'migrations/007_add_index.up.sql', sql: 'SELECT 1;' }], {
      downMigrations: ['migrations/007_add_index.down.sql'],
    });
    expect(f.filter((x) => x.ruleId === 'DA006')).toHaveLength(0);
  });
});

describe('DA007 SET NOT NULL without backfill marker', () => {
  it('fires without marker', () => {
    const f = analyzeMigrations([
      one('ALTER TABLE users ALTER COLUMN email SET NOT NULL;'),
    ]);
    expect(ids(f)).toContain('DA007');
  });

  it('does not fire when marker comment precedes', () => {
    const f = analyzeMigrations([
      one('-- devagent:backfilled\nALTER TABLE users ALTER COLUMN email SET NOT NULL;'),
    ]);
    expect(f.filter((x) => x.ruleId === 'DA007')).toHaveLength(0);
  });
});

describe('DA008 CONCURRENTLY inside transaction', () => {
  it('fires inside BEGIN/COMMIT', () => {
    const sql = 'BEGIN;\nCREATE INDEX CONCURRENTLY idx_a ON t (a);\nCOMMIT;';
    const f = analyzeMigrations([one(sql)]);
    expect(ids(f)).toContain('DA008');
  });

  it('does not fire outside a transaction block', () => {
    const f = analyzeMigrations([one('CREATE INDEX CONCURRENTLY idx_a ON t (a);')]);
    expect(f.filter((x) => x.ruleId === 'DA008')).toHaveLength(0);
  });
});

describe('masking and multi-line handling', () => {
  it('ignores keywords inside block comments and string literals across lines', () => {
    const sql = [
      '/*',
      'DROP TABLE legacy;',
      '*/',
      "UPDATE notes SET body = 'see DROP COLUMN old_col discussion' WHERE id = 1;",
    ].join('\n');
    expect(analyzeMigrations([one(sql)])).toHaveLength(0);
  });

  it('detects multi-line statements spanning newlines', () => {
    const sql = [
      'ALTER TABLE invoices',
      '  ALTER COLUMN total TYPE smallint,',
      '  ADD COLUMN memo text;',
    ].join('\n');
    const found = ids(analyzeMigrations([one(sql)]));
    expect(found).toContain('DA002');
    expect(found).not.toContain('DA005');
  });

  it('reports line numbers pointing into the original file', () => {
    const sql = ['SELECT 1;', '', 'DROP TABLE users;'].join('\n');
    const f = analyzeMigrations([one(sql)]).find((x) => x.ruleId === 'DA001');
    expect(f?.line).toBe(3);
  });
});
