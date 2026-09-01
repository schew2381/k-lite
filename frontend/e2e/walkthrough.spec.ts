// These tests replay the demo walkthrough (boot, watch traffic, block it by
// name, scale, kill, drain) and assert the UI tells the sim's story.

import { expect, type Page, test } from '@playwright/test'

// Traffic beats every 4 sim-seconds and one story plays at a time, so tests
// that wait on specific calls crank the sim to 4× first.
async function fourX(page: Page) {
  await page.getByRole('radio', { name: 'Four times speed' }).click()
}

test('cluster boots: three nodes, five Ready instances, a traced call flowing', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByTestId('node-node-1')).toBeVisible()
  await expect(page.getByTestId('node-node-2')).toBeVisible()
  await expect(page.getByTestId('node-node-3')).toBeVisible()
  await expect(page.locator('[data-testid^="instance-"][data-phase="Ready"]')).toHaveCount(5)
  await fourX(page)
  await expect(page.getByTestId('rail-allowed').or(page.getByTestId('rail-denied')).first()).toBeVisible(
    {
      timeout: 60_000,
    },
  )
  await expect(page.getByTestId('trace-step-active')).toBeVisible({ timeout: 60_000 })
})

test('the etcd browser lists live keys and expands them to YAML', async ({ page }) => {
  await page.goto('/#/etcd')
  await expect(page.getByTestId('etcd-rev')).toBeVisible()
  const bRow = page.getByTestId('etcd-row').filter({ hasText: '/klite/v1/workloads/b' })
  await expect(bRow).toHaveCount(1)
  await bRow.click()
  await expect(bRow).toContainText('replicas: 2')
  await expect(page.getByTestId('etcd-row').filter({ hasText: '/klite/v1/instances/b-1' })).toHaveCount(
    1,
  )
})

test('the infra pod inspector shows what the control plane programmed', async ({ page }) => {
  await page.goto('/')
  await expect(page.locator('[data-testid^="instance-"][data-phase="Ready"]')).toHaveCount(5)
  await page.getByTestId('infra-node-1').click()
  const sheet = page.getByTestId('infra-sheet')
  await expect(sheet).toContainText('b.svc.klite')
  await expect(sheet).toContainText('cluster b')
  await expect(sheet).toContainText('default: allow')
  await expect(sheet).toContainText('b-1 READY')
  await expect(sheet).toContainText('10.44.128.') // identity map rows
  await expect(page.getByTestId('control-plane')).toContainText('held: scheduler')
})

test('a named policy blocks matching calls and the rail says so', async ({ page }) => {
  await page.goto('/')
  await fourX(page)
  const combos = page.getByTestId('policy-builder').getByRole('combobox')
  await combos.nth(1).click()
  await page.getByRole('option', { name: 'a', exact: true }).click()
  await combos.nth(2).click()
  await page.getByRole('option', { name: 'c', exact: true }).click()
  await page.getByTestId('policy-apply').click()

  // denials carry the policy's name, and a → b stays green
  const denied = page.getByTestId('rail-denied').filter({ hasText: 'blocked by deny-a-to-c' })
  await expect(denied.first()).toBeVisible({ timeout: 110_000 })
  await expect(page.getByTestId('rail-denied').filter({ hasText: 'a-1 → b' })).toHaveCount(0)

  // the simulator widget agrees and names the rule
  const sim = page.getByTestId('policy-sim')
  await sim.getByRole('combobox').first().click()
  await page.getByRole('option', { name: 'a', exact: true }).click()
  await sim.getByRole('combobox').nth(1).click()
  await page.getByRole('option', { name: 'c', exact: true }).click()
  await sim.getByRole('button', { name: 'check' }).click()
  const verdict = page.getByTestId('policy-verdict')
  await expect(verdict).toHaveAttribute('data-allowed', 'false')
  await expect(verdict).toContainText('deny-a-to-c')

  // a → b is untouched, and the same evaluator says so
  await sim.getByRole('combobox').nth(1).click()
  await page.getByRole('option', { name: 'b', exact: true }).click()
  await sim.getByRole('button', { name: 'check' }).click()
  await expect(verdict).toHaveAttribute('data-allowed', 'true')

  // deleting the policy reopens traffic
  await page.getByRole('button', { name: 'Delete policy deny-a-to-c' }).click()
  await expect(page.getByTestId('policy-builder')).not.toContainText('deny-a-to-c')
})

test('scaling b up from the service card lands a third Ready instance', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByTestId('replicas-b')).toHaveText('×2')
  await page.getByTestId('scale-up-b').click()
  await expect(page.getByTestId('replicas-b')).toHaveText('×3')
  await expect(page.locator('[data-testid="instance-b-3"][data-phase="Ready"]')).toBeVisible()
  // and back down: the newest instance drains away
  await page.getByTestId('scale-down-b').click()
  await expect(page.getByTestId('instance-b-3')).toHaveCount(0, { timeout: 30_000 })
})

test('killing an instance brings it back with a restart tally', async ({ page }) => {
  await page.goto('/')
  const chip = page.getByTestId('instance-b-1')
  await chip.hover()
  await chip.getByRole('button', { name: 'Kill b-1' }).click()
  // Failed lasts only the first backoff (500ms), so assert the durable outcome
  await expect(chip).toContainText('↻1', { timeout: 15_000 })
  await expect(chip).toHaveAttribute('data-phase', 'Ready', { timeout: 15_000 })
})

test('draining a node migrates its instances and removes the card', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('button', { name: 'Actions for node-3' }).click()
  await page.getByRole('menuitem', { name: 'Drain & remove' }).click()
  await expect(page.getByTestId('node-node-3')).toHaveCount(0, { timeout: 60_000 })
  // capacity is back to full elsewhere, and nothing is left Pending
  await expect(page.locator('[data-testid^="instance-"][data-phase="Ready"]')).toHaveCount(5)
  await expect(page.getByTestId('pending-tray')).toHaveCount(0)
})

test('reduced motion: no dots, but the rail still narrates', async ({ browser }) => {
  const context = await browser.newContext({ reducedMotion: 'reduce' })
  const page = await context.newPage()
  await page.goto('/')
  await fourX(page) // reduced motion drops the dots, not the sim clock
  await expect(page.getByTestId('rail-allowed').first()).toBeVisible({ timeout: 60_000 })
  expect(await page.locator('.traffic-dot').count()).toBe(0)
  await context.close()
})

test('a service created from the dialog schedules and gets kdns records everywhere', async ({
  page,
}) => {
  await page.goto('/')
  await fourX(page)
  await page.getByTestId('add-service').click()
  await page.getByTestId('new-service-name').fill('d')
  await page.getByTestId('new-service-create').click()
  await expect(page.getByTestId('service-d')).toBeVisible()
  await expect(page.locator('[data-testid^="instance-d-"][data-phase="Ready"]')).toHaveCount(2, {
    timeout: 30_000,
  })
  // every node's infra pod now serves the new name
  for (const node of ['node-1', 'node-2', 'node-3']) {
    await expect(page.getByTestId(`infra-${node}`)).toContainText('d.svc.klite')
  }
})

test('live flow: flights run fast and untraced, the panel keeps the latest call', async ({ page }) => {
  await page.goto('/')
  await fourX(page)
  await page.getByTestId('flow-toggle').getByRole('radio', { name: 'Live flow' }).click()
  await expect(page.locator('.traffic-dot').first()).toBeVisible({ timeout: 60_000 })
  // no step-by-step walkthrough in live flow
  expect(await page.getByTestId('trace-step-active').count()).toBe(0)
  // pause the sim so no new flights spawn: the airborne ones land in seconds,
  // not the traced half-minute
  await page.getByRole('radio', { name: 'Pause' }).click()
  await expect(page.locator('.traffic-dot')).toHaveCount(0, { timeout: 15_000 })
  await expect(page.getByTestId('trace-panel')).toContainText('latest call')
  await expect(page.getByTestId('trace-panel').locator('ol li').first()).toBeVisible()
})
