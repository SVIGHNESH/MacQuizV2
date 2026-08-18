/**
 * MacQuiz Feedback Form generator.
 *
 * How to use:
 * 1. Go to https://script.google.com and create a new project.
 * 2. Paste this file in, save, and run createFeedbackForm() (authorize when prompted).
 * 3. Open the Logs (View > Logs) to get the shareable form link and the edit link.
 *
 * The form is deliberately short: 2 required questions, everything else optional,
 * so users can finish it in under a minute.
 */
function createFeedbackForm() {
  var form = FormApp.create('MacQuiz Feedback')
    .setDescription(
      'Help us improve MacQuiz. Takes less than a minute - only the first two questions are required.'
    )
    .setCollectEmail(false)
    .setAllowResponseEdits(true)
    .setConfirmationMessage('Thanks! Your feedback goes straight to the MacQuiz team.');

  // Q1: who they are (required - responses mean different things per role)
  form.addMultipleChoiceItem()
    .setTitle('How do you use MacQuiz?')
    .setChoiceValues(['Student (taking quizzes)', 'Teacher (creating and running quizzes)', 'Admin'])
    .setRequired(true);

  // Q2: overall satisfaction (required, single tap)
  form.addScaleItem()
    .setTitle('Overall, how happy are you with MacQuiz?')
    .setBounds(1, 5)
    .setLabels('Not happy', 'Very happy')
    .setRequired(true);

  // Q3: what needs improving most (optional, checkboxes so one tap each)
  form.addCheckboxItem()
    .setTitle('Which areas should we improve first?')
    .setChoiceValues([
      'Taking a quiz (timer, navigation, submitting answers)',
      'Creating and scheduling quizzes',
      'Results and analytics dashboards',
      'Live monitoring during a quiz',
      'Mobile / phone experience',
      'Speed and reliability',
      'Login and account management',
      'Look and feel of the interface',
    ])
    .setRequired(false);

  // Q4: free text - the one open question (optional)
  form.addParagraphTextItem()
    .setTitle('What is the one thing you would change or add?')
    .setHelpText('A sentence or two is plenty.')
    .setRequired(false);

  // Q5: optional contact for follow-up
  form.addTextItem()
    .setTitle('Email (optional, only if you are okay with us following up)')
    .setRequired(false);

  Logger.log('Share this link with users: ' + form.getPublishedUrl());
  Logger.log('Edit the form here: ' + form.getEditUrl());
  return form;
}

/**
 * Optional: run this once instead of createFeedbackForm() if you also want
 * responses collected into a new Google Sheet.
 */
function createFeedbackFormWithSheet() {
  var form = createFeedbackForm();
  var sheet = SpreadsheetApp.create('MacQuiz Feedback (Responses)');
  form.setDestination(FormApp.DestinationType.SPREADSHEET, sheet.getId());
  Logger.log('Responses sheet: ' + sheet.getUrl());
}
