import { App as AntApp, Col, Form, Row } from 'antd';
import { useCallback, useState } from 'react';

import { costPeriods, costRates, priceBooks, usageAgreement } from '../api/control';
import { usePoll } from '../hooks';
import { ControlPlaneAlert } from '../parts/control-plane';
import type { CostRate, PriceBookInput } from '../types';
import { Agreement } from './cost/Agreement';
import { PeriodsCard } from './cost/PeriodsCard';
import { PriceBooksCard } from './cost/PriceBooksCard';
import type { BookForm } from './cost/PriceBooksCard';
import { RatesCard } from './cost/RatesCard';
import { Report } from './cost/Report';

/**
 * The cost pool (P8): what a GPU-hour costs, and what each tenant is billed for
 * holding one whether they used it or not.
 *
 * Idle capacity is allocated, not excluded. That is the whole argument: a GPU
 * that sits idle is a fixed cost that has to land on somebody's invoice, and a
 * platform that bills only the busy minutes hides the problem in the operator's
 * own margin instead of showing it as the utilization problem it is.
 */
export function Cost() {
  const { message } = AntApp.useApp();
  const rates$ = usePoll((signal) => costRates.list(signal), 30000);
  const books$ = usePoll((signal) => priceBooks.list(signal), 30000);
  const periods$ = usePoll((signal) => costPeriods.list(signal), 30000);
  const agreement$ = usePoll((signal) => usageAgreement.get(signal), 30000);
  const [open, setOpen] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [rateForm] = Form.useForm<{ cluster: string; gpuHour: number; currency: string }>();
  const [bookForm] = Form.useForm<BookForm>();
  const [closeForm] = Form.useForm<{ period: string }>();

  // Fetched only when a period is picked. Returning undefined rather than
  // calling with an undefined id keeps the hook simple: it always runs, and the
  // result is absent rather than an error nobody asked for.
  const report$ = usePoll(
    async (signal) => (open ? costPeriods.get(open, signal) : undefined),
    0,
  );

  const refreshRates = rates$.refresh;
  const refreshBooks = books$.refresh;
  const refreshPeriods = periods$.refresh;

  const saveRate = useCallback(
    async (values: CostRate) => {
      setBusy(true);
      try {
        await costRates.save(values);
        rateForm.resetFields();
        refreshRates();
      } catch (err) {
        message.error(`cannot declare the rate: ${err}`);
      } finally {
        setBusy(false);
      }
    },
    [rateForm, refreshRates, message],
  );

  const saveBook = useCallback(
    async (values: PriceBookInput) => {
      setBusy(true);
      try {
        await priceBooks.save(values);
        bookForm.resetFields();
        refreshBooks();
      } catch (err) {
        message.error(`cannot declare the price: ${err}`);
      } finally {
        setBusy(false);
      }
    },
    [bookForm, refreshBooks, message],
  );

  const close = useCallback(
    async (values: { period: string }) => {
      setBusy(true);
      try {
        await costPeriods.close(values.period);
        message.success(`${values.period} is priced and stored.`);
        refreshPeriods();
        setOpen(values.period);
        closeForm.resetFields();
      } catch (err) {
        message.error(`cannot close ${values.period}: ${err}`);
      } finally {
        setBusy(false);
      }
    },
    [closeForm, refreshPeriods, message],
  );

  return (
    <>
      {(rates$.error || periods$.error || books$.error) && (
        <ControlPlaneAlert
          error={rates$.error || periods$.error || books$.error}
          style={{ marginBottom: 16 }}
          hint="Rates, prices, periods and allocations come from fleet-apiserver with a database behind it."
        />
      )}

      <Row gutter={16}>
        <Col xs={24} lg={12}>
          <RatesCard rates={rates$.data ?? []} form={rateForm} busy={busy} onSubmit={saveRate} />
        </Col>
        <Col xs={24} lg={12}>
          <PeriodsCard
            periods={periods$.data ?? []}
            open={open}
            form={closeForm}
            busy={busy}
            onPick={setOpen}
            onSubmit={close}
          />
        </Col>
      </Row>

      {/* Full width on purpose: five fields beside a half-width card wrapped
          into two ragged rows with one input stranded on the first, and the
          third column of the table above it had no room either. */}
      <div style={{ marginTop: 16 }}>
        <PriceBooksCard books={books$.data ?? []} form={bookForm} busy={busy} onSubmit={saveBook} />
      </div>

      {open && report$.data && <Report report={report$.data} />}

      <Agreement report={agreement$.data} />
    </>
  );
}